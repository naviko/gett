// Package term handles everything printed to the terminal: safe output,
// colors, and progress bars.
package term

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/mattn/go-isatty"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

var (
	Verbose bool
	Silent  bool
)

// Setup applies the output mode.
func Setup(verbose, silent bool) {
	Verbose, Silent = verbose, silent
}

func tty(f *os.File) bool {
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// StdoutIsTTY overrides detection (tests).
var StdoutIsTTY *bool

func StdoutTTY() bool {
	if StdoutIsTTY != nil {
		return *StdoutIsTTY
	}
	return tty(os.Stdout)
}
func StderrTTY() bool { return tty(os.Stderr) }

// Sanitize strips every control character, so a server cannot inject
// terminal escape sequences into a single-line field.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// SanitizeText is Sanitize for multi-line text: newlines and tabs survive.
func SanitizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Info prints a status line to stderr unless silent.
func Info(format string, a ...any) {
	if Silent {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// Debug prints to stderr only in verbose mode.
func Debug(format string, a ...any) {
	if !Verbose {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// Progress is a container of bars. A nil-backed Progress is a no-op, which
// is what you get when stderr is not a terminal or silent mode is on.
type Progress struct{ p *mpb.Progress }

func NewProgress() *Progress {
	if Silent || !StderrTTY() {
		return &Progress{}
	}
	return &Progress{p: mpb.New(
		mpb.WithOutput(os.Stderr),
		mpb.WithWidth(30),
		mpb.WithRefreshRate(150*time.Millisecond),
	)}
}

// Wait blocks until every bar is complete or aborted.
func (p *Progress) Wait() {
	if p.p != nil {
		p.p.Wait()
	}
}

// Shutdown tears the container down even if bars are unfinished.
func (p *Progress) Shutdown() {
	if p.p != nil {
		p.p.Shutdown()
	}
}

type Bar struct{ b *mpb.Bar }

// Bar adds a bar. total <= 0 means unknown size.
func (p *Progress) Bar(name string, total int64, removeWhenDone bool) *Bar {
	if p.p == nil {
		return &Bar{}
	}
	name = Sanitize(name)
	if len(name) > 32 {
		name = name[:29] + "..."
	}
	var opts []mpb.BarOption
	if removeWhenDone {
		opts = append(opts, mpb.BarRemoveOnComplete())
	}
	if total <= 0 {
		opts = append(opts,
			mpb.PrependDecorators(decor.Name(name+" ", decor.WC{C: decor.DindentRight}), decor.CurrentKibiByte("% .1f")),
			mpb.AppendDecorators(decor.AverageSpeed(decor.SizeB1024(0), " % .1f")),
		)
		return &Bar{p.p.New(0, mpb.SpinnerStyle(), opts...)}
	}
	opts = append(opts,
		mpb.PrependDecorators(decor.Name(name+" ", decor.WC{C: decor.DindentRight}), decor.CountersKibiByte("% .1f / % .1f")),
		mpb.AppendDecorators(decor.Percentage(decor.WC{W: 5}), decor.AverageSpeed(decor.SizeB1024(0), " % .1f"), decor.AverageETA(decor.ET_STYLE_GO, decor.WC{W: 6})),
	)
	return &Bar{p.p.New(total, mpb.BarStyle(), opts...)}
}

func (b *Bar) Add(n int) {
	if b.b != nil {
		b.b.IncrBy(n)
	}
}

func (b *Bar) SetCurrent(n int64) {
	if b.b != nil {
		b.b.SetCurrent(n)
	}
}

// Done marks the bar complete at whatever it reached.
func (b *Bar) Done() {
	if b.b != nil && !b.b.Completed() {
		b.b.SetTotal(-1, true)
	}
}

func (b *Bar) Abort() {
	if b.b != nil {
		b.b.Abort(true)
	}
}

// Writer wraps w so every write advances the bar.
func (b *Bar) Writer(w io.Writer) io.Writer { return &countWriter{w, b} }

type countWriter struct {
	w io.Writer
	b *Bar
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.b.Add(n)
	return n, err
}

// Reader wraps r so every read advances the bar.
func (b *Bar) Reader(r io.Reader) io.Reader { return &countReader{r, b} }

type countReader struct {
	r io.Reader
	b *Bar
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.b.Add(n)
	return n, err
}
