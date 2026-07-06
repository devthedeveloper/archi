package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Orchestrator runs agents with bounded concurrency and a run-level budget.
type Orchestrator struct {
	Concurrency int    // default 3; ARCHI_AGENT_CONCURRENCY overrides
	RunBudget   int    // total output-token budget; ARCHI_RUN_TOKENS, default 150000
	Trace       *Trace // never nil; nopTrace when tracing disabled
	Board       *Board // live UI; nil when piped

	mu       sync.Mutex // guards the counters below
	started  time.Time
	calls    int
	tokensIn int
	spent    int // output tokens charged so far
}

// AgentCall pairs an agent with its input for parallel execution.
type AgentCall struct {
	Agent Agent
	Input TaskInput
}

// errRunBudget marks a call skipped because the run token budget is spent.
var errRunBudget = errors.New("run token budget exhausted")

// defaultAgentConcurrency / defaultRunTokens are the env-knob fallbacks.
const defaultAgentConcurrency = 3
const defaultRunTokens = 150000

// newOrchestrator builds the Phase 1 orchestrator for a repo root: env-derived
// knobs, a live trace under .archi/runs (nop + warn() on create failure), nil
// Board. Sets started = time.Now().
func newOrchestrator(root string) *Orchestrator {
	tr, err := newTrace(root, time.Now())
	if err != nil {
		warn("trace disabled: " + err.Error())
		tr = nopTrace()
	}
	return &Orchestrator{
		Concurrency: agentConcurrency(),
		RunBudget:   runTokenBudget(),
		Trace:       tr,
		started:     time.Now(),
	}
}

// agentConcurrency resolves ARCHI_AGENT_CONCURRENCY (mirrors contextBudget).
func agentConcurrency() int {
	if v := strings.TrimSpace(os.Getenv("ARCHI_AGENT_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		warn("ignoring invalid ARCHI_AGENT_CONCURRENCY=" + v)
	}
	return defaultAgentConcurrency
}

// runTokenBudget resolves ARCHI_RUN_TOKENS (mirrors contextBudget).
func runTokenBudget() int {
	if v := strings.TrimSpace(os.Getenv("ARCHI_RUN_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		warn("ignoring invalid ARCHI_RUN_TOKENS=" + v)
	}
	return defaultRunTokens
}

// Run executes one agent call, charging the run budget, updating the board, and
// appending to the trace. Returns the result even on error.
func (o *Orchestrator) Run(ctx context.Context, a Agent, in TaskInput) AgentResult {
	return o.runStream(ctx, a, in, nil, nil)
}

// runStream is Run plus live streaming: chunks stream to live (stdout for the
// designer's fast path) and token progress updates sp's suffix, exactly as
// runModel does today. The caller owns sp's lifecycle (start/Stop/Abort);
// runStream only calls SetSuffix.
func (o *Orchestrator) runStream(ctx context.Context, a Agent, in TaskInput, live io.Writer, sp *Spinner) AgentResult {
	sys, err := renderSystem(a.Spec, in)
	if err != nil {
		res := AgentResult{Role: a.Spec.Role, Err: err}
		o.Trace.record(res, a.P.Model())
		return res
	}
	user := agentUserMessage(in)

	// Budget admission, atomic. Admission-then-charge means one in-flight call
	// may overshoot the budget; accepted since streams can't be pre-sized.
	o.mu.Lock()
	if o.RunBudget > 0 && o.spent >= o.RunBudget {
		o.mu.Unlock()
		o.Board.Set(a.Spec.Role, "failed", "budget exhausted")
		warn(string(a.Spec.Role) + ": skipped — run token budget exhausted (raise ARCHI_RUN_TOKENS)")
		res := AgentResult{Role: a.Spec.Role, Err: errRunBudget}
		o.Trace.record(res, a.P.Model())
		return res
	}
	o.mu.Unlock()

	o.Board.Set(a.Spec.Role, "running", "")
	start := time.Now()
	chars := 0
	progress := funcWriter(func(b []byte) (int, error) {
		if live != nil {
			live.Write(b)
		}
		chars += len(b)
		if sp != nil {
			sp.SetSuffix(fmt.Sprintf("~%s tokens", humanCount((chars+3)/4)))
		}
		if o.Board != nil {
			o.Board.Set(a.Spec.Role, "running", "~"+humanCount((chars+3)/4)+" tokens")
		}
		return len(b), nil
	})
	out, cerr := a.P.Complete(ctx, sys, user, progress)

	res := AgentResult{
		Role:      a.Spec.Role,
		Output:    out,
		TokensIn:  estimateTokens(sys) + estimateTokens(user),
		TokensOut: estimateTokens(out),
		Duration:  time.Since(start),
		Err:       cerr,
	}
	o.mu.Lock()
	o.calls++
	o.tokensIn += res.TokensIn
	o.spent += res.TokensOut
	o.mu.Unlock()
	o.Trace.record(res, a.P.Model())

	if cerr != nil {
		o.Board.Set(a.Spec.Role, "failed", cerr.Error())
	} else {
		o.Board.Set(a.Spec.Role, "done", fmt.Sprintf("~%s tokens · %s", humanCount(res.TokensOut), res.Duration.Round(time.Second)))
	}
	return res
}

// RunParallel executes independent calls with the worker pool; ctx cancels all
// on first hard error. Order of results matches order of calls. Budget skips do
// not cancel healthy siblings.
func (o *Orchestrator) RunParallel(ctx context.Context, calls []AgentCall) []AgentResult {
	n := o.Concurrency
	if n <= 0 {
		n = defaultAgentConcurrency
	}
	results := make([]AgentResult, len(calls))
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var once sync.Once
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c AgentCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if gctx.Err() != nil {
				results[i] = AgentResult{Role: c.Agent.Spec.Role, Err: gctx.Err()}
				return
			}
			results[i] = o.Run(gctx, c.Agent, c.Input)
			if results[i].Err != nil && !errors.Is(results[i].Err, errRunBudget) {
				once.Do(cancel)
			}
		}(i, c)
	}
	wg.Wait()
	return results
}

// costLine renders the end-of-run summary, "" when no calls ran.
func (o *Orchestrator) costLine() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.calls == 0 {
		return ""
	}
	plural := "s"
	if o.calls == 1 {
		plural = ""
	}
	line := fmt.Sprintf("%d agent call%s · ~%s in / ~%s out tokens · %s",
		o.calls, plural, humanCount(o.tokensIn), humanCount(o.spent), time.Since(o.started).Round(time.Second))
	if o.Trace.Dir() != "" {
		line += dim("  ·  trace " + o.Trace.Dir())
	}
	return line
}
