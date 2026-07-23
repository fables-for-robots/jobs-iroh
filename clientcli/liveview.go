package clientcli

// The liveView + xferProgress terminal plumbing, ported from jobs'
// internal/jobscli/liveview.go. Additions over the upstream file: paint()
// (SGR color, gated on TTY + NO_COLOR/TERM=dumb — jobs had no color at all)
// and an ANSI-aware truncateLine so painted lines still never wrap.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v2"
	"golang.org/x/term"
)

// liveView renders build/transfer progress to stderr. On a TTY it maintains
// one in-place block (cursor-up + clear-to-end ANSI, lines truncated to the
// terminal width so wrapping never breaks the cursor arithmetic); on a
// non-TTY every method degrades to plain append-only lines (Update: no-op).
// A nil *liveView is a silent no-op, so plain callers/tests can pass nil.
// No cursor-hiding, no alt-screen — an interrupt can never leave the
// terminal in a broken state.
type liveView struct {
	mu    sync.Mutex
	w     io.Writer
	isTTY bool
	color bool
	width int
	drawn int // lines in the currently drawn block
}

func newLiveView(f *os.File) *liveView {
	v := &liveView{w: f, width: 100}
	if term.IsTerminal(int(f.Fd())) {
		v.isTTY = true
		v.color = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			v.width = w
		}
	}
	return v
}

// cliLiveView builds a command's live view over its error stream: TTY-aware
// on the real stderr, plain non-TTY over a test-configured ErrWriter. Live
// progress always goes to stderr — stdout stays machine-readable (keys,
// tables).
func cliLiveView(c *cli.Context) *liveView {
	if w := c.App.ErrWriter; w != nil {
		if f, ok := w.(*os.File); !ok || f != os.Stderr {
			return &liveView{w: w, width: 100}
		}
	}
	return newLiveView(os.Stderr)
}

func (v *liveView) IsTTY() bool { return v != nil && v.isTTY }

// SGR codes for paint.
const (
	sgrGreen = "32"
	sgrRed   = "31"
)

// paint wraps s in an SGR color when the view is a color-capable TTY;
// plain s otherwise (non-TTY, NO_COLOR set, TERM=dumb, nil view).
func (v *liveView) paint(code, s string) string {
	if v == nil || !v.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Update redraws the live block (TTY only).
func (v *liveView) Update(lines []string) {
	if v == nil || !v.isTTY {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.drawn == 0 && len(lines) == 0 {
		return
	}
	var b strings.Builder
	if v.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dA\x1b[J", v.drawn)
	}
	for _, l := range lines {
		b.WriteString(truncateLine(l, v.width))
		b.WriteByte('\n')
	}
	io.WriteString(v.w, b.String())
	v.drawn = len(lines)
}

// Println prints a permanent line, erasing the live block first so the block
// always stays below permanent output.
func (v *liveView) Println(line string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.eraseLocked()
	fmt.Fprintln(v.w, line)
}

// Collapse erases the live block and, when final is non-empty, prints it as
// the block's permanent one-line summary.
func (v *liveView) Collapse(final string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.eraseLocked()
	if final != "" {
		fmt.Fprintln(v.w, final)
	}
}

func (v *liveView) eraseLocked() {
	if v.isTTY && v.drawn > 0 {
		fmt.Fprintf(v.w, "\x1b[%dA\x1b[J", v.drawn)
		v.drawn = 0
	}
}

// truncateLine truncates s to width printable runes so a block line never
// wraps (wrapping would break the cursor-up arithmetic). ANSI CSI sequences
// pass through uncounted; a truncated line that contained any is closed with
// a reset so color never leaks past the ellipsis.
func truncateLine(s string, width int) string {
	if width <= 0 || visibleLen(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	r := []rune(s)
	var b strings.Builder
	printed, sawCSI := 0, false
	for i := 0; i < len(r) && printed < width-1; i++ {
		if j, ok := csiEnd(r, i); ok {
			b.WriteString(string(r[i:j]))
			sawCSI = true
			i = j - 1
			continue
		}
		b.WriteRune(r[i])
		printed++
	}
	b.WriteString("…")
	if sawCSI {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// csiEnd reports whether r[i] starts an ANSI CSI sequence ("\x1b[" … final
// byte in @–~) and returns the index one past its end.
func csiEnd(r []rune, i int) (int, bool) {
	if r[i] != '\x1b' || i+1 >= len(r) || r[i+1] != '[' {
		return 0, false
	}
	j := i + 2
	for j < len(r) && (r[j] < 0x40 || r[j] > 0x7e) {
		j++
	}
	if j < len(r) {
		j++
	}
	return j, true
}

// visibleLen counts printable runes, skipping CSI sequences.
func visibleLen(s string) int {
	r := []rune(s)
	n := 0
	for i := 0; i < len(r); i++ {
		if j, ok := csiEnd(r, i); ok {
			i = j - 1
			continue
		}
		n++
	}
	return n
}

func formatDur(d time.Duration) string { return d.Round(time.Second).String() }

// formatBytes renders n as a compact decimal size (B, kB, MB, GB).
func formatBytes(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d B", n)
}

// xferProgress adapts an amber push/pull progress callback (concurrent-safe)
// to the liveView: TTY gets an in-place "[push] 312/412 objects" line,
// non-TTY one line per crossed quarter (known totals only). finish()
// collapses to a "[push] 412 objects · 3s" summary.
type xferProgress struct {
	lv    *liveView
	label string
	start time.Time

	mu       sync.Mutex
	done     int
	total    int
	bytes    int64 // payload bytes moved; shown in the summary when known
	quarters int   // last quarter printed (non-TTY)
}

func newXferProgress(lv *liveView, label string) *xferProgress {
	return &xferProgress{lv: lv, label: label, start: time.Now()}
}

func (x *xferProgress) cb(done, total int) {
	x.mu.Lock()
	x.done, x.total = done, total
	line := fmt.Sprintf("[%s] %d objects", x.label, done)
	if total > 0 {
		line = fmt.Sprintf("[%s] %d/%d objects", x.label, done, total)
	}
	var quarterLine string
	if !x.lv.IsTTY() && total > 0 {
		if q := 4 * done / total; q > x.quarters {
			x.quarters = q
			quarterLine = fmt.Sprintf("%s (%d%%)", line, q*25)
		}
	}
	x.mu.Unlock()
	if x.lv.IsTTY() {
		x.lv.Update([]string{line})
	} else if quarterLine != "" {
		x.lv.Println(quarterLine)
	}
}

// setBytes records the transfer's payload byte total (available only from the
// final stats, not the live callback) for finish's summary.
func (x *xferProgress) setBytes(n int64) {
	x.mu.Lock()
	x.bytes = n
	x.mu.Unlock()
}

// finish collapses the block: a summary line on success (when the callback
// ever fired), a plain erase on failure or a no-op transfer.
func (x *xferProgress) finish(ok bool) {
	x.mu.Lock()
	n := x.total
	if x.done > n {
		n = x.done
	}
	b := x.bytes
	x.mu.Unlock()
	if !ok || n == 0 {
		x.lv.Collapse("")
		return
	}
	sum := fmt.Sprintf("[%s] %d objects", x.label, n)
	if b > 0 {
		sum += " · " + formatBytes(b)
	}
	x.lv.Collapse(sum + " · " + formatDur(time.Since(x.start)))
}
