package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ashdaily/spoofy/internal/engine"
)

func snapshot() engine.Snapshot {
	return engine.Snapshot{
		Elapsed:     90 * time.Second,
		Total:       12345,
		Errors:      7,
		AchievedRPS: 19.4,
		P50:         16 * time.Millisecond,
		P95:         218 * time.Millisecond,
		ByClass: map[string]int64{
			"2xx": 12000, "4xx": 300, "5xx": 38, "error": 7,
		},
	}
}

// A bytes.Buffer is not a terminal, so the renderer must choose log mode. This
// is the path that runs under Kubernetes, where ANSI redraw sequences in a pod
// log are noise people then have to work around.
func TestNonTerminalOutputIsPlainAndGreppable(t *testing.T) {
	buf := &bytes.Buffer{}
	live := NewLive(buf, "http://staging:8080", "constant 20/s", time.Millisecond)

	live.render(snapshot(), 20, true)
	out := buf.String()

	if strings.Contains(out, "\033[") {
		t.Errorf("log mode emitted ANSI escape sequences:\n%q", out)
	}
	if lines := strings.Count(strings.TrimSpace(out), "\n"); lines != 0 {
		t.Errorf("expected exactly one line, got %d extra newlines", lines)
	}

	// Everything an operator would grep for has to be present as key=value.
	for _, want := range []string{
		"sent=12345", "rate=19.4/s", "target=20.0/s",
		"p50=16ms", "p95=218ms",
		"2xx=12000", "4xx=300", "5xx=38", "err=7",
		"target_up=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log line missing %q\n  got: %s", want, out)
		}
	}
}

func TestTargetDownIsVisibleInLogMode(t *testing.T) {
	buf := &bytes.Buffer{}
	live := NewLive(buf, "http://staging:8080", "constant 20/s", time.Millisecond)

	live.render(snapshot(), 20, false)

	if !strings.Contains(buf.String(), "target_up=false") {
		t.Error("a down target must be visible in log output")
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1204857, "1,204,857"},
	}
	for _, tc := range tests {
		if got := formatCount(tc.in); got != tc.want {
			t.Errorf("formatCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatLatency(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "–"},
		{450 * time.Microsecond, "450µs"},
		{16 * time.Millisecond, "16ms"},
		{999 * time.Millisecond, "999ms"},
		{1500 * time.Millisecond, "1.50s"},
	}
	for _, tc := range tests {
		if got := formatLatency(tc.in); got != tc.want {
			t.Errorf("formatLatency(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m 30s"},
		{3661 * time.Second, "1h 1m"},
		{26 * time.Hour, "26h 0m"},
	}
	for _, tc := range tests {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The panel is only rendered at a terminal, but it must not panic on any input
// — including the all-zero snapshot present for the first half second of every
// single run.
func TestPanelHandlesAnEmptySnapshot(t *testing.T) {
	live := NewLive(&bytes.Buffer{}, "http://localhost:8080", "constant 10/s", time.Millisecond)
	live.tty = true

	out := live.panel(engine.Snapshot{ByClass: map[string]int64{}}, 0, true)

	if !strings.Contains(out, "spoofy") {
		t.Error("panel should always identify itself")
	}
	if !strings.Contains(out, "http://localhost:8080") {
		t.Error("panel should show the target")
	}
}

func TestPanelShowsShapeAndTarget(t *testing.T) {
	live := NewLive(&bytes.Buffer{}, "https://staging.acme.com", "diurnal, averaging 20/s", time.Millisecond)
	live.tty = true

	out := live.panel(snapshot(), 20, true)

	for _, want := range []string{"staging.acme.com", "diurnal", "ctrl-c"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel missing %q", want)
		}
	}
}

// Redrawing must move the cursor up by exactly the number of lines previously
// drawn; getting it wrong smears frames down the terminal.
func TestTerminalRedrawRewindsExactlyOneFrame(t *testing.T) {
	buf := &bytes.Buffer{}
	live := NewLive(buf, "http://localhost:8080", "constant 10/s", time.Millisecond)
	live.tty = true

	live.render(snapshot(), 10, true)
	firstFrameLines := live.linesDrawn
	if firstFrameLines == 0 {
		t.Fatal("first frame drew nothing")
	}

	buf.Reset()
	live.render(snapshot(), 10, true)

	want := "\033[" + itoa(firstFrameLines) + "A"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("second frame did not rewind %d lines (looking for %q)", firstFrameLines, want)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

func TestRunStopsWhenDoneCloses(t *testing.T) {
	buf := &bytes.Buffer{}
	live := NewLive(buf, "http://localhost:8080", "constant 10/s", time.Millisecond)

	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		live.Run(done, func() (engine.Snapshot, float64, bool) {
			return snapshot(), 10, true
		})
		close(finished)
	}()

	close(done)

	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after done was closed")
	}

	// A final frame should have been emitted so the last numbers stay on screen.
	if buf.Len() == 0 {
		t.Error("no final frame was rendered")
	}
}
