// Package client executes generated requests and reports what happened.
//
// The interesting behaviour here is what it does when the target is unhealthy.
// A load tester that hammers a failing service is doing its job; a daemon that
// does the same is an amplifier for someone else's outage, and it runs for
// weeks. So the client backs off when the target stops answering, and
// publishes whether it believes the target is up.
package client

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	// failureThreshold is how many consecutive failures before backing off.
	// A couple of blips during a rolling deploy should not change behaviour;
	// a sustained outage should.
	failureThreshold = 3

	// maxBackoff caps the wait. Beyond about half a minute, a longer wait adds
	// nothing but makes recovery detection sluggish — and noticing that the
	// target came back is the whole point.
	maxBackoff = 30 * time.Second

	baseBackoff = 250 * time.Millisecond

	// maxBodyBytes bounds how much of a response is read. The body is discarded
	// anyway; reading it exists only so the connection can be reused. Without a
	// cap, one endpoint returning a huge payload would dominate memory for the
	// life of the daemon.
	maxBodyBytes = 64 << 10
)

// Result describes one completed request.
type Result struct {
	// OperationID, Method, and Path identify what was attempted. Path is the
	// templated form ("/pets/{petId}"), never the concrete URL — metrics label
	// on it, and concrete URLs would make cardinality unbounded.
	OperationID string
	Method      string
	Path        string

	StatusCode int
	Duration   time.Duration
	BytesRead  int64

	// Err is set for transport-level failures: connection refused, DNS, TLS,
	// timeout. An HTTP error response is not an Err — a 500 means the target
	// answered, which is a different fact from the target being unreachable.
	Err error
}

// OK reports whether the request produced a non-error HTTP response.
func (r Result) OK() bool { return r.Err == nil && r.StatusCode < 400 }

// StatusClass buckets the status for metrics: "2xx", "4xx", "error", etc.
func (r Result) StatusClass() string {
	switch {
	case r.Err != nil:
		return "error"
	case r.StatusCode >= 500:
		return "5xx"
	case r.StatusCode >= 400:
		return "4xx"
	case r.StatusCode >= 300:
		return "3xx"
	case r.StatusCode >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// Client sends requests and tracks target health.
//
// It is safe for concurrent use by many workers.
type Client struct {
	http *http.Client

	consecutiveFailures atomic.Int64
	up                  atomic.Bool

	// sleep is injectable so backoff behaviour is testable without waiting.
	sleep func(context.Context, time.Duration)
}

// New returns a Client with a transport tuned for a long-running daemon.
func New(timeout time.Duration, concurrency int) *Client {
	if concurrency < 1 {
		concurrency = 1
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Idle connections must at least match concurrency, or workers repeatedly
	// open and discard sockets. Over weeks that shows up as port exhaustion on
	// the host rather than as anything visible in the tool.
	transport.MaxIdleConns = concurrency * 2
	transport.MaxIdleConnsPerHost = concurrency * 2
	transport.IdleConnTimeout = 90 * time.Second

	c := &Client{
		http:  &http.Client{Timeout: timeout, Transport: transport},
		sleep: sleepCtx,
	}
	c.up.Store(true) // assume healthy until told otherwise
	return c
}

// Up reports whether the target is currently believed to be reachable.
func (c *Client) Up() bool { return c.up.Load() }

// ConsecutiveFailures reports the current failure streak.
func (c *Client) ConsecutiveFailures() int64 { return c.consecutiveFailures.Load() }

// Do sends the request, backing off first if the target has been failing.
func (c *Client) Do(ctx context.Context, req *http.Request, op OperationRef) Result {
	if wait := c.backoff(); wait > 0 {
		c.sleep(ctx, wait)
		if ctx.Err() != nil {
			return Result{
				OperationID: op.ID, Method: op.Method, Path: op.Path,
				Err: ctx.Err(),
			}
		}
	}

	started := time.Now()
	resp, err := c.http.Do(req)
	elapsed := time.Since(started)

	result := Result{
		OperationID: op.ID,
		Method:      op.Method,
		Path:        op.Path,
		Duration:    elapsed,
	}

	if err != nil {
		// A cancelled context is shutdown, not a target failure; counting it
		// would mark a healthy target down every time someone stops the daemon.
		if errors.Is(err, context.Canceled) {
			result.Err = err
			return result
		}
		result.Err = err
		c.recordFailure()
		return result
	}
	defer resp.Body.Close()

	// Drain so the connection can be reused, but never unboundedly.
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	result.StatusCode = resp.StatusCode
	result.BytesRead = n

	// The target answered. Even a 500 means it is up — that distinction is why
	// a 5xx does not trigger backoff. Spoofy exists partly to observe 5xx
	// rates, so backing off on them would suppress the signal it was deployed
	// to produce.
	c.recordSuccess()
	return result
}

// OperationRef carries the identity of what is being requested, so Result can
// be labelled without the client depending on the openapi package.
type OperationRef struct {
	ID     string
	Method string
	Path   string
}

func (c *Client) recordFailure() {
	n := c.consecutiveFailures.Add(1)
	if n >= failureThreshold {
		c.up.Store(false)
	}
}

func (c *Client) recordSuccess() {
	c.consecutiveFailures.Store(0)
	c.up.Store(true)
}

// backoff returns how long to wait before the next attempt, growing
// exponentially with the failure streak.
func (c *Client) backoff() time.Duration {
	n := c.consecutiveFailures.Load()
	if n < failureThreshold {
		return 0
	}

	steps := n - failureThreshold
	if steps > 20 { // guard the shift below
		steps = 20
	}
	wait := time.Duration(float64(baseBackoff) * math.Pow(2, float64(steps)))
	if wait > maxBackoff || wait <= 0 {
		wait = maxBackoff
	}
	return wait
}

func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
