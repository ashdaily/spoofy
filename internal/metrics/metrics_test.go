package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ashdaily/spoofy/internal/client"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}

func TestRecordExportsRequestCounters(t *testing.T) {
	m := New()

	m.Record(client.Result{
		Method: "GET", Path: "/pets/{petId}",
		StatusCode: 200, Duration: 12 * time.Millisecond, BytesRead: 128,
	})
	m.Record(client.Result{
		Method: "GET", Path: "/pets/{petId}",
		StatusCode: 500, Duration: 40 * time.Millisecond,
	})

	body := scrape(t, m)

	for _, want := range []string{
		`spoofy_requests_total{class="2xx",method="GET",path="/pets/{petId}",status="200"} 1`,
		`spoofy_requests_total{class="5xx",method="GET",path="/pets/{petId}",status="500"} 1`,
		`spoofy_request_duration_seconds_count{method="GET",path="/pets/{petId}"} 2`,
		`spoofy_response_bytes_total{path="/pets/{petId}"} 128`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing:\n  %s", want)
		}
	}
}

// Labelling on concrete URLs instead of templated paths is how a monitoring
// stack gets taken down by its own traffic generator. The templated form must
// survive into the exported label verbatim.
func TestPathLabelStaysTemplated(t *testing.T) {
	m := New()

	// The same logical endpoint hit with many different ids.
	for i := 0; i < 500; i++ {
		m.Record(client.Result{
			Method: "GET", Path: "/pets/{petId}",
			StatusCode: 200, Duration: time.Millisecond,
		})
	}

	body := scrape(t, m)

	var seriesCount int
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "spoofy_requests_total{") {
			seriesCount++
		}
	}
	if seriesCount != 1 {
		t.Errorf("500 requests produced %d time series, want 1", seriesCount)
	}
	if !strings.Contains(body, `path="/pets/{petId}"`) {
		t.Error("the templated path label did not survive into the export")
	}
}

func TestTransportErrorsAreBucketedNotFreeText(t *testing.T) {
	m := New()

	// Every one of these carries a unique address or message. If the label were
	// the error string, each would create its own time series — a cardinality
	// bomb that arrives precisely when the network is already misbehaving.
	errs := []error{
		errors.New("dial tcp 10.0.0.1:8080: connect: connection refused"),
		errors.New("dial tcp 10.0.0.2:8080: connect: connection refused"),
		errors.New("dial tcp 10.0.0.3:8080: connect: connection refused"),
		context.DeadlineExceeded,
		errors.New(`Get "http://x": net/http: request canceled (Client.Timeout exceeded)`),
		errors.New("lookup nosuchhost: no such host"),
		errors.New("read tcp 1.2.3.4:80: connection reset by peer"),
	}
	for _, err := range errs {
		m.Record(client.Result{Method: "GET", Path: "/pets", Err: err})
	}

	body := scrape(t, m)

	var kinds int
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "spoofy_errors_total{") {
			kinds++
			if strings.Contains(line, "10.0.0.") {
				t.Errorf("an address leaked into a metric label: %s", line)
			}
		}
	}
	if kinds == 0 {
		t.Fatal("no error metrics were exported")
	}
	if kinds > 5 {
		t.Errorf("%d distinct error labels from %d errors; they are not being bucketed", kinds, len(errs))
	}

	for _, want := range []string{"connection_refused", "timeout", "dns", "connection_reset"} {
		if !strings.Contains(body, `kind="`+want+`"`) {
			t.Errorf("expected an error bucket %q", want)
		}
	}
}

func TestErrorKindBuckets(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{errors.New("connect: connection refused"), "connection_refused"},
		{errors.New("lookup foo: no such host"), "dns"},
		{errors.New("connection reset by peer"), "connection_reset"},
		{errors.New("unexpected EOF"), "eof"},
		{errors.New("tls: handshake failure"), "tls"},
		{errors.New("x509: certificate signed by unknown authority"), "tls"},
		{errors.New("something entirely new"), "other"},
	}
	for _, tc := range tests {
		if got := errorKind(tc.err); got != tc.want {
			t.Errorf("errorKind(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestGaugesReflectState(t *testing.T) {
	m := New()

	m.SetTargetUp(false)
	if !strings.Contains(scrape(t, m), "spoofy_target_up 0") {
		t.Error("target_up should be 0 when down")
	}

	m.SetTargetUp(true)
	m.SetTargetRate(42.5)
	m.IncInFlight()
	m.IncInFlight()
	m.DecInFlight()

	body := scrape(t, m)
	for _, want := range []string{
		"spoofy_target_up 1",
		"spoofy_target_rate 42.5",
		"spoofy_requests_in_flight 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

// Build failures are a persistent spec problem, not the target misbehaving.
// Keeping them out of the HTTP error rate stops a bad spec from looking like a
// bad deploy.
func TestBuildErrorsAreCountedSeparately(t *testing.T) {
	m := New()
	m.RecordBuildError()
	m.RecordBuildError()

	body := scrape(t, m)
	if !strings.Contains(body, "spoofy_request_build_errors_total 2") {
		t.Error("build errors were not exported")
	}
	if strings.Contains(body, "spoofy_errors_total") {
		t.Error("build errors must not land in the transport error counter")
	}
}

// Runtime metrics belong on the same endpoint: the answer to "is memory flat
// after a week" should not require a second exporter.
func TestRuntimeMetricsAreExported(t *testing.T) {
	body := scrape(t, New())
	for _, want := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected runtime metric %q on the endpoint", want)
		}
	}
}

func TestServeExposesMetricsAndHealth(t *testing.T) {
	m := New()
	m.Record(client.Result{Method: "GET", Path: "/pets", StatusCode: 200, Duration: time.Millisecond})

	addr := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- m.Serve(ctx, addr) }()

	base := "http://" + addr
	waitForServer(t, base+"/healthz")

	t.Run("healthz", func(t *testing.T) {
		body, code := httpGet(t, base+"/healthz")
		if code != http.StatusOK {
			t.Errorf("status = %d", code)
		}
		if !strings.Contains(body, "ok") {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("metrics", func(t *testing.T) {
		body, code := httpGet(t, base+"/metrics")
		if code != http.StatusOK {
			t.Errorf("status = %d", code)
		}
		if !strings.Contains(body, "spoofy_requests_total") {
			t.Error("metrics endpoint did not expose spoofy metrics")
		}
	})

	t.Run("root redirects to metrics", func(t *testing.T) {
		client := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		resp, err := client.Get(base + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("status = %d, want 302", resp.StatusCode)
		}
	})

	// Shutdown must be clean, not an error.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve returned an error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("Serve did not return after cancellation")
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never came up", url)
}

func httpGet(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode
}

func TestStatusTextIsBounded(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{200, "200"}, {404, "404"}, {503, "503"},
		{0, "unknown"}, {-1, "unknown"}, {9999, "unknown"},
	}
	for _, tc := range tests {
		if got := statusText(tc.code); got != tc.want {
			t.Errorf("statusText(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestItoa(t *testing.T) {
	for _, n := range []int{0, 7, 42, 200, 503, 12345} {
		if got, want := itoa(n), fmt.Sprint(n); got != want {
			t.Errorf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}
