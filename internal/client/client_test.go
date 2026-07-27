package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ref() OperationRef {
	return OperationRef{ID: "listPets", Method: "GET", Path: "/pets"}
}

func get(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestDoRecordsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(5*time.Second, 4)
	got := c.Do(context.Background(), get(t, srv.URL), ref())

	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if got.StatusCode != 200 {
		t.Errorf("StatusCode = %d", got.StatusCode)
	}
	if got.BytesRead != int64(len(`{"ok":true}`)) {
		t.Errorf("BytesRead = %d", got.BytesRead)
	}
	if got.Duration <= 0 {
		t.Error("Duration should be positive")
	}
	if !got.OK() {
		t.Error("OK() should be true for a 200")
	}
	if got.OperationID != "listPets" || got.Path != "/pets" {
		t.Errorf("operation identity not carried through: %+v", got)
	}
}

func TestStatusClass(t *testing.T) {
	tests := []struct {
		result Result
		want   string
	}{
		{Result{StatusCode: 200}, "2xx"},
		{Result{StatusCode: 201}, "2xx"},
		{Result{StatusCode: 301}, "3xx"},
		{Result{StatusCode: 404}, "4xx"},
		{Result{StatusCode: 500}, "5xx"},
		{Result{StatusCode: 503}, "5xx"},
		{Result{Err: context.DeadlineExceeded}, "error"},
	}
	for _, tc := range tests {
		if got := tc.result.StatusClass(); got != tc.want {
			t.Errorf("StatusClass(%+v) = %q, want %q", tc.result, got, tc.want)
		}
	}
}

// An HTTP error response means the target answered. That is a different fact
// from the target being unreachable, and conflating them would make Spoofy
// suppress exactly the 5xx signal it was deployed to produce.
func TestErrorResponsesDoNotMarkTargetDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(5*time.Second, 4)
	for i := 0; i < 10; i++ {
		got := c.Do(context.Background(), get(t, srv.URL), ref())
		if got.Err != nil {
			t.Fatalf("a 500 should not be a transport error: %v", got.Err)
		}
		if got.StatusCode != 500 {
			t.Fatalf("StatusCode = %d", got.StatusCode)
		}
	}

	if !c.Up() {
		t.Error("target should still be considered up after 10 500s")
	}
	if c.ConsecutiveFailures() != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", c.ConsecutiveFailures())
	}
}

func TestUnreachableTargetMarksDownAfterThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c := New(500*time.Millisecond, 2)
	c.sleep = func(context.Context, time.Duration) {} // do not actually wait

	if !c.Up() {
		t.Fatal("a fresh client should assume the target is up")
	}

	for i := 0; i < failureThreshold; i++ {
		got := c.Do(context.Background(), get(t, url), ref())
		if got.Err == nil {
			t.Fatalf("request %d unexpectedly succeeded against a closed server", i)
		}
	}

	if c.Up() {
		t.Errorf("target should be marked down after %d consecutive failures", failureThreshold)
	}
}

func TestRecoveryResetsHealth(t *testing.T) {
	var serving atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serving.Load() {
			// Simulate a redeploy: the handler hijacks and drops the connection.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(2*time.Second, 2)
	c.sleep = func(context.Context, time.Duration) {}

	for i := 0; i < failureThreshold+2; i++ {
		c.Do(context.Background(), get(t, srv.URL), ref())
	}
	if c.Up() {
		t.Fatal("expected the target to be marked down")
	}

	// The target comes back, as it does after every rolling deploy.
	serving.Store(true)
	got := c.Do(context.Background(), get(t, srv.URL), ref())
	if got.Err != nil {
		t.Fatalf("request after recovery failed: %v", got.Err)
	}
	if !c.Up() {
		t.Error("target should be marked up again after a success")
	}
	if c.ConsecutiveFailures() != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 after recovery", c.ConsecutiveFailures())
	}
}

func TestBackoffGrowsThenCaps(t *testing.T) {
	c := New(time.Second, 1)

	// Below the threshold there is no wait at all: a blip during a rolling
	// deploy should not change behaviour.
	for i := 0; i < failureThreshold; i++ {
		if got := c.backoff(); got != 0 {
			t.Errorf("with %d failures backoff = %v, want 0", i, got)
		}
		c.recordFailure()
	}

	var previous time.Duration
	for i := 0; i < 12; i++ {
		got := c.backoff()
		if got <= 0 {
			t.Fatalf("backoff should be positive past the threshold, got %v", got)
		}
		if got > maxBackoff {
			t.Fatalf("backoff %v exceeded the %v cap", got, maxBackoff)
		}
		if got < previous {
			t.Fatalf("backoff shrank from %v to %v", previous, got)
		}
		previous = got
		c.recordFailure()
	}

	if previous != maxBackoff {
		t.Errorf("backoff settled at %v, want the %v cap", previous, maxBackoff)
	}
}

// Backing off must not mean ignoring shutdown. A daemon that takes 30 seconds
// to notice SIGTERM fails its k8s termination grace period and gets killed.
func TestBackoffIsInterruptedByCancellation(t *testing.T) {
	c := New(time.Second, 1)
	for i := 0; i < failureThreshold+5; i++ {
		c.recordFailure()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	c.sleep(ctx, maxBackoff)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("sleep ignored cancellation, took %v", elapsed)
	}
}

func TestCancelledContextIsNotCountedAsTargetFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	c := New(5*time.Second, 1)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	got := c.Do(ctx, get(t, srv.URL).WithContext(ctx), ref())
	if got.Err == nil {
		t.Fatal("expected an error from the cancelled request")
	}
	if c.ConsecutiveFailures() != 0 {
		t.Errorf("shutdown was counted as a target failure (%d); every stop would mark a healthy target down",
			c.ConsecutiveFailures())
	}
}

// A single enormous response must not be read into memory unbounded — over
// weeks that is the difference between a flat RSS and an OOM kill.
func TestResponseBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("x", 8<<10)
		for i := 0; i < 64; i++ { // 512 KiB, well past the cap
			w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	c := New(5*time.Second, 1)
	got := c.Do(context.Background(), get(t, srv.URL), ref())

	if got.BytesRead > maxBodyBytes {
		t.Errorf("read %d bytes, cap is %d", got.BytesRead, maxBodyBytes)
	}
}

func TestConcurrentUse(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(5*time.Second, 16)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if got := c.Do(context.Background(), get(t, srv.URL), ref()); got.Err != nil {
					t.Errorf("unexpected error: %v", got.Err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := served.Load(); got != 320 {
		t.Errorf("server saw %d requests, want 320", got)
	}
	if !c.Up() {
		t.Error("target should be up")
	}
}
