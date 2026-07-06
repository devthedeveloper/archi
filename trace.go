package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Trace appends one JSON line per agent call to .archi/runs/<stamp>/trace.jsonl
// and writes each agent's full output to .archi/runs/<stamp>/<n>-<role>.md.
// Failures to write warn() and continue — tracing never fails the run.
type Trace struct {
	dir string // "" means nop
	mu  sync.Mutex
	seq int
}

// traceEvent is one JSON line in trace.jsonl.
type traceEvent struct {
	Seq       int       `json:"seq"`
	Role      string    `json:"role"`
	Model     string    `json:"model"`
	TokensIn  int       `json:"tokens_in"`
	TokensOut int       `json:"tokens_out"`
	Ms        int64     `json:"ms"`
	Verdict   string    `json:"verdict,omitempty"`
	Err       string    `json:"err,omitempty"`
	Time      time.Time `json:"time"`
}

// maxRuns caps how many run directories are retained under .archi/runs.
const maxRuns = 20

// runsDir returns the trace directory for root.
func runsDir(root string) string { return filepath.Join(archiDir(root), "runs") }

// newTrace creates .archi/runs/<stamp>/ and prunes old runs to maxRuns. Returns
// an error only when the run dir can't be made.
func newTrace(root string, t time.Time) (*Trace, error) {
	dir := filepath.Join(runsDir(root), t.Format(stampLayout))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	pruneRuns(runsDir(root), maxRuns)
	return &Trace{dir: dir}, nil
}

// nopTrace returns a Trace that records nothing (dir == "").
func nopTrace() *Trace { return &Trace{} }

// Dir returns the run directory, "" for a nop trace.
func (t *Trace) Dir() string { return t.dir }

// record appends res as a trace event (model is the provider's model id) and
// writes res.Output to <seq>-<role>.md. Safe for concurrent use. All write
// failures warn() and continue; nop traces return immediately.
func (t *Trace) record(res AgentResult, model string) {
	if t.dir == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	ev := traceEvent{
		Seq:       t.seq,
		Role:      string(res.Role),
		Model:     model,
		TokensIn:  res.TokensIn,
		TokensOut: res.TokensOut,
		Ms:        res.Duration.Milliseconds(),
		Verdict:   statusOrEmpty(res.Verdict),
		Err:       errStringOrEmpty(res.Err),
		Time:      time.Now().UTC(),
	}
	if line, err := json.Marshal(ev); err != nil {
		warn("trace: " + err.Error())
	} else if f, err := os.OpenFile(filepath.Join(t.dir, "trace.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		warn("trace: " + err.Error())
	} else {
		if _, err := f.Write(append(line, '\n')); err != nil {
			warn("trace: " + err.Error())
		}
		f.Close()
	}
	outPath := filepath.Join(t.dir, fmt.Sprintf("%02d-%s.md", t.seq, res.Role))
	if err := os.WriteFile(outPath, []byte(res.Output), 0o644); err != nil {
		warn("trace: " + err.Error())
	}
}

// pruneRuns removes the oldest run directories so at most keep remain. Lexical
// order of stampLayout names is chronological. Non-directories and unremovable
// entries are skipped with a warn().
func pruneRuns(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) <= keep {
		return
	}
	sort.Strings(dirs)
	for _, name := range dirs[:len(dirs)-keep] {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			warn("trace: could not prune " + name + ": " + err.Error())
		}
	}
}

// statusOrEmpty returns the verdict status string, or "" for a nil verdict.
func statusOrEmpty(v *Verdict) string {
	if v == nil {
		return ""
	}
	return string(v.Status)
}

// errStringOrEmpty returns err.Error(), or "" for a nil error.
func errStringOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
