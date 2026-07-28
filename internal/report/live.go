// Package report renders what Spoofy is doing, for humans.
//
// Two modes, because there are two audiences. At a terminal it redraws a
// compact panel in place. Piped to a file or running under Kubernetes it emits
// one line per interval, since ANSI redraw sequences in a pod log are noise
// people then have to grep around.
package report

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"

	"github.com/ashdaily/spoofy/internal/engine"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	valueStyle = lipgloss.NewStyle().Bold(true)

	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	brandStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
)

// Live renders periodic progress.
type Live struct {
	out      io.Writer
	tty      bool
	target   string
	shape    string
	interval time.Duration

	linesDrawn int
}

// NewLive returns a renderer writing to out. TTY detection decides the mode.
func NewLive(out io.Writer, target, shape string, interval time.Duration) *Live {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return &Live{
		out:      out,
		tty:      isTerminal(out),
		target:   target,
		shape:    shape,
		interval: interval,
	}
}

func isTerminal(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// Run redraws until done is closed.
func (l *Live) Run(done <-chan struct{}, snapshot func() (engine.Snapshot, float64, bool)) {
	// A half-second cadence would produce 170,000 log lines a day.
	interval := l.interval
	if !l.tty {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			snap, rate, up := snapshot()
			l.finish(snap, rate, up)
			return
		case <-ticker.C:
			snap, rate, up := snapshot()
			l.render(snap, rate, up)
		}
	}
}

func (l *Live) render(s engine.Snapshot, targetRate float64, up bool) {
	if !l.tty {
		fmt.Fprintln(l.out, l.logLine(s, targetRate, up))
		return
	}

	// Move back over the previous frame instead of clearing the screen, so
	// scrollback above the panel survives.
	if l.linesDrawn > 0 {
		fmt.Fprintf(l.out, "\033[%dA", l.linesDrawn)
	}

	panel := l.panel(s, targetRate, up)
	lines := strings.Split(panel, "\n")
	for _, line := range lines {
		fmt.Fprintf(l.out, "\033[2K%s\n", line)
	}
	l.linesDrawn = len(lines)
}

// finish leaves a final frame in place and stops redrawing over it.
func (l *Live) finish(s engine.Snapshot, targetRate float64, up bool) {
	l.render(s, targetRate, up)
	l.linesDrawn = 0
	if l.tty {
		fmt.Fprintln(l.out)
	}
}

func (l *Live) panel(s engine.Snapshot, targetRate float64, up bool) string {
	status := okStyle.Render("●")
	if !up {
		status = errStyle.Render("●")
	}

	header := fmt.Sprintf("%s %s  %s   %s",
		brandStyle.Render("spoofy"),
		status,
		titleStyle.Render(l.target),
		dimStyle.Render(l.shape),
	)

	left := []string{
		row("rate", fmt.Sprintf("%.1f/s", s.AchievedRPS), dimStyle.Render(fmt.Sprintf("target %.1f/s", targetRate))),
		row("sent", formatCount(s.Total), ""),
		row("uptime", formatDuration(s.Elapsed), ""),
	}

	right := []string{
		row("p50", formatLatency(s.P50), ""),
		row("p95", formatLatency(s.P95), ""),
		row("ok", fmt.Sprintf("%.1f%%", s.SuccessRate()*100), ""),
	}

	classes := []string{
		classRow("2xx", s.ByClass["2xx"], okStyle),
		classRow("4xx", s.ByClass["4xx"], warnStyle),
		classRow("5xx", s.ByClass["5xx"], errStyle),
		classRow("err", s.ByClass["error"], errStyle),
	}

	columns := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(30).Render(strings.Join(left, "\n")),
		lipgloss.NewStyle().Width(20).Render(strings.Join(right, "\n")),
		lipgloss.NewStyle().Render(strings.Join(classes, "\n")),
	)

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("  " + header + "\n\n")
	for _, line := range strings.Split(columns, "\n") {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString(dimStyle.Render("  ctrl-c to stop"))
	return b.String()
}

func row(label, value, suffix string) string {
	out := fmt.Sprintf("%s %s", labelStyle.Render(pad(label, 7)), valueStyle.Render(value))
	if suffix != "" {
		out += " " + suffix
	}
	return out
}

func classRow(label string, count int64, style lipgloss.Style) string {
	if count == 0 {
		return fmt.Sprintf("%s %s", labelStyle.Render(pad(label, 4)), dimStyle.Render("0"))
	}
	return fmt.Sprintf("%s %s", labelStyle.Render(pad(label, 4)), style.Render(formatCount(count)))
}

// logLine is the non-TTY form: one greppable line, no escape sequences.
func (l *Live) logLine(s engine.Snapshot, targetRate float64, up bool) string {
	return fmt.Sprintf(
		"spoofy uptime=%s sent=%d rate=%.1f/s target=%.1f/s ok=%.1f%% p50=%s p95=%s 2xx=%d 4xx=%d 5xx=%d err=%d target_up=%t",
		formatDuration(s.Elapsed), s.Total, s.AchievedRPS, targetRate, s.SuccessRate()*100,
		formatLatency(s.P50), formatLatency(s.P95),
		s.ByClass["2xx"], s.ByClass["4xx"], s.ByClass["5xx"], s.ByClass["error"], up,
	)
}

func pad(s string, width int) string {
	for len(s) < width {
		s += " "
	}
	return s
}

func formatCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	// 1,204,857 is scannable at a glance; 1204857 is not.
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

func formatLatency(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60

	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
