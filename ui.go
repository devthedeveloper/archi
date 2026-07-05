package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// The UI writes all chrome to stderr so stdout stays a clean document that can
// be piped or redirected. Color is enabled only for an interactive stderr with
// NO_COLOR unset.
var (
	stderrTTY = isTTY(os.Stderr)
	stdoutTTY = isTTY(os.Stdout)
	// Color on an interactive stderr, or when forced (CLICOLOR_FORCE), unless
	// NO_COLOR is set — both are widely honored conventions.
	useColor = (stderrTTY || os.Getenv("CLICOLOR_FORCE") != "") && os.Getenv("NO_COLOR") == ""
)

// 24-bit ANSI. The accent is Go's brand cyan (#00ADD8) so archi feels at home
// next to the toolchain it's built on.
func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func cyan(s string) string   { return paint("38;2;0;173;216", s) }
func green(s string) string  { return paint("38;2;83;158;67", s) }
func red(s string) string    { return paint("38;2;207;50;62", s) }
func yellow(s string) string { return paint("38;2;220;170;0", s) }
func bold(s string) string   { return paint("1", s) }
func dim(s string) string    { return paint("2", s) }

// banner prints the archi wordmark and tagline to stderr. Restraint over
// ASCII-art noise: a clean accent line reads as considered, not decorated.
func banner() {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  "+bold(cyan("▚ archi"))+"  "+dim("· an architect that knows your codebase"))
	fmt.Fprintln(os.Stderr, "  "+dim(strings.Repeat("─", 48)))
}

// step prints a bullet line for a completed or in-progress action.
func step(sym, msg string) { fmt.Fprintln(os.Stderr, "  "+sym+" "+msg) }

func ok(msg string)   { step(green("✓"), msg) }
func info(msg string) { step(cyan("▸"), msg) }
func warn(msg string) { step(yellow("!"), msg) }

func failf(format string, a ...any) {
	fmt.Fprintln(os.Stderr, "  "+red("✗")+" "+fmt.Sprintf(format, a...))
	os.Exit(1)
}

// langBars renders a compact horizontal bar chart of languages by token weight.
// showFiles includes the per-language file count in the trailing detail.
func langBars(stats []langStat, totalTokens int, showFiles bool) {
	if totalTokens == 0 {
		return
	}
	const width = 24
	const max = 6
	for i, s := range stats {
		if i >= max {
			fmt.Fprintln(os.Stderr, "  "+dim(fmt.Sprintf("  … and %d more", len(stats)-max)))
			break
		}
		filled := s.tokens * width / totalTokens
		if filled == 0 {
			filled = 1
		}
		bar := cyan(strings.Repeat("█", filled)) + dim(strings.Repeat("·", width-filled))
		detail := "~" + humanCount(s.tokens)
		if showFiles {
			detail = fmt.Sprintf("%d files · ~%s", s.files, humanCount(s.tokens))
		}
		fmt.Fprintf(os.Stderr, "  %-12s %s %s\n", s.name, bar, dim(detail))
	}
}

// Spinner animates a single stderr line while a slow step runs. On a
// non-interactive stderr it degrades to a single printed message.
type Spinner struct {
	msg    string
	mu     sync.Mutex
	suffix string
	stop   chan struct{}
	done   chan struct{}
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func startSpinner(msg string) *Spinner {
	s := &Spinner{msg: msg, stop: make(chan struct{}), done: make(chan struct{})}
	if !stderrTTY {
		fmt.Fprintln(os.Stderr, "  "+cyan("▸")+" "+msg+" "+dim("…"))
		close(s.done)
		return s
	}
	go s.run()
	return s
}

func (s *Spinner) run() {
	defer close(s.done)
	t := time.NewTicker(90 * time.Millisecond)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			suf := s.suffix
			s.mu.Unlock()
			frame := cyan(spinnerFrames[i%len(spinnerFrames)])
			line := "  " + frame + " " + s.msg
			if suf != "" {
				line += "  " + dim(suf)
			}
			fmt.Fprint(os.Stderr, "\r"+line+"\x1b[K")
			i++
		}
	}
}

// SetSuffix updates the trailing detail (e.g. a live token count).
func (s *Spinner) SetSuffix(suffix string) {
	s.mu.Lock()
	s.suffix = suffix
	s.mu.Unlock()
}

// clear stops the animation and wipes the spinner line.
func (s *Spinner) clear() {
	if !stderrTTY {
		return
	}
	close(s.stop)
	<-s.done
	fmt.Fprint(os.Stderr, "\r\x1b[K")
}

// Stop ends the animation and prints a final ✓ line.
func (s *Spinner) Stop(okMsg string) {
	s.clear()
	ok(okMsg)
}

// Abort ends the animation without a success line (used on error).
func (s *Spinner) Abort() { s.clear() }
