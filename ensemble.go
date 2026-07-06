package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ensembleOpts is the resolved -agents/-critic-model/-rounds configuration.
type ensembleOpts struct {
	mode        string // "full", "fast", or "off" (off == fast in Phase 2)
	rounds      int    // designer↔critic debate rounds, clamped 1..3
	criticModel string // "" => design model
}

// resolveEnsembleOpts applies precedence flag > .archi config > default
// (full, 1, ""). An invalid -agents value aborts with the valid set.
func resolveEnsembleOpts(cfg *Config, agents, criticModel string, rounds int) ensembleOpts {
	o := ensembleOpts{mode: "full", rounds: 1}
	if cfg.Agents != "" {
		o.mode = cfg.Agents
	}
	if cfg.Rounds != 0 {
		o.rounds = cfg.Rounds
	}
	if cfg.CriticModel != "" {
		o.criticModel = cfg.CriticModel
	}
	if agents != "" {
		o.mode = agents
	}
	if criticModel != "" {
		o.criticModel = criticModel
	}
	if rounds != 0 {
		o.rounds = rounds
	}
	switch o.mode {
	case "full", "fast", "off":
	default:
		failf("invalid -agents %q (valid: full, fast, off)", o.mode)
	}
	if o.rounds < 1 {
		o.rounds = 1
	}
	if o.rounds > 3 {
		o.rounds = 3
	}
	return o
}

// persistEnsembleFlags writes the explicitly-passed ensemble flags into the
// config so they load as defaults next time. Advisory: a save error warns and
// the run continues.
func persistEnsembleFlags(cfg *Config, root string, setAgents, setCritic, setRounds bool, agents, criticModel string, rounds int) {
	changed := false
	if setAgents {
		cfg.Agents = agents
		changed = true
	}
	if setCritic {
		cfg.CriticModel = criticModel
		changed = true
	}
	if setRounds {
		cfg.Rounds = rounds
		changed = true
	}
	if changed {
		if err := cfg.save(root); err != nil {
			warn("could not save ensemble settings: " + err.Error())
		}
	}
}

// designEnsemble runs explorer → designer(streamed) → critic panel → revision
// loop, returning the final document with a Review notes section appended.
// Progress is on stderr; the document streams to live when non-nil.
func (s *session) designEnsemble(request, interview string, live io.Writer) (string, error) {
	// 1. Explorer (optional, big repos only).
	brief := ""
	if explorerWanted(s.cfg) {
		sp := startSpinner("Exploring the codebase")
		b, n, err := runExplorer(s.ctx, s.orch, s.mk, s.root, s.profile, s.fileMap, request, interview)
		if err != nil {
			sp.Abort()
			warn("explorer unavailable — using standard grounding")
		} else {
			sp.Stop(fmt.Sprintf("Explored — brief from %d files", n))
			brief = b
		}
	}

	// 2. Designer (streamed to stdout exactly as fast mode, plus the brief).
	req := request
	if interview != "" {
		req = request + "\n\nClarifications from the requester:\n" + interview
	}
	designer := s.agentFor(RoleDesigner, "")
	info("Designing …")
	if live != nil {
		os.Stderr.WriteString("\n")
	}
	res := s.orch.runStream(s.ctx, designer,
		designTask(request, designUserMessage(s.profile, s.fileMap, brief, s.focus, req)), live, nil)
	if res.Err != nil {
		return "", res.Err
	}
	doc := res.Output

	// 3. Critic panel + revision loop.
	critics := make([]Agent, len(criticRoles))
	for i, role := range criticRoles {
		critics[i] = Agent{Spec: criticSpec(role), P: s.criticMk(1500)}
	}
	base := TaskInput{Request: request, Interview: interview, Grounding: s.criticGrounding(brief)}

	var lastResults []AgentResult
	finalRound := 0
	revised := false
	for round := 1; round <= s.opts.rounds; round++ {
		results := s.runPanel(critics, base, doc)
		lastResults = results
		finalRound = round
		if !hasBlocking(results) {
			break
		}
		info("Revising with review feedback …")
		if live != nil {
			os.Stderr.WriteString("\n")
		}
		rres := s.orch.runStream(s.ctx, designer,
			designTask(request, revisionUserMessage(doc, formatVerdictsForRevision(results))), live, nil)
		if rres.Err != nil {
			warn("revision skipped: " + rres.Err.Error())
			warn("blocking review findings were not resolved — see Review notes")
			revised = false
			break
		}
		doc = rres.Output
		revised = true
	}

	s.printReviewSummary(lastResults, revised)
	notes := "\n\n" + renderReviewNotes(lastResults, finalRound, s.opts.rounds, revised)
	if live != nil {
		io.WriteString(live, notes) // so the notes appear on stdout in live mode too
	}
	return doc + notes, nil
}

// criticGrounding builds the shared grounding for the panel, fit to budget.
func (s *session) criticGrounding(brief string) []budgetPart {
	parts := []budgetPart{
		{name: "codebase profile", required: true, content: s.profile},
		{name: "file map", content: s.fileMap},
	}
	if brief != "" {
		parts = append(parts, budgetPart{name: "grounding brief", content: brief})
	}
	fitted, _ := fitBudget(parts, contextBudget())
	return fitted
}

// runPanel runs the critic panel with the live Board (TTY) and prints a
// one-line verdict summary per critic afterward.
func (s *session) runPanel(critics []Agent, base TaskInput, doc string) []AgentResult {
	info("Review panel …")
	board := newBoard(criticRoles)
	s.orch.Board = board
	results := runCriticPanel(s.ctx, s.orch, critics, base, doc)
	board.Stop()
	s.orch.Board = nil
	for i, r := range results {
		printCriticLine(criticRoles[i], r)
	}
	return results
}

// runCriticPanel runs the critics in a bounded pool, parses their verdicts
// (downgrading pathless grounding findings), and warns once if the run budget
// skipped any. Results preserve criticRoles order. Unlike RunParallel, one
// critic's failure never cancels the others — the panel tolerates each critic
// independently (spec: "a critic errs ⇒ run continues").
func runCriticPanel(ctx context.Context, o *Orchestrator, critics []Agent, base TaskInput, design string) []AgentResult {
	n := o.Concurrency
	if n <= 0 {
		n = defaultAgentConcurrency
	}
	results := make([]AgentResult, len(critics))
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i, c := range critics {
		wg.Add(1)
		go func(i int, c Agent) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			in := base
			in.Extra = map[string]string{"design": design}
			results[i] = o.Run(ctx, c, in)
		}(i, c)
	}
	wg.Wait()
	skipped := 0
	for i := range results {
		if results[i].Err != nil {
			if errors.Is(results[i].Err, errRunBudget) {
				skipped++
			}
			continue
		}
		v, _ := parseVerdict(results[i].Output)
		if critics[i].Spec.Role == RoleGrndCritic {
			downgradePathless(v)
		}
		results[i].Verdict = v
	}
	if skipped > 0 {
		warn(fmt.Sprintf("run budget exhausted — skipped %d critics", skipped))
	}
	return results
}

// hasBlocking reports whether any successfully-parsed verdict is blocking.
func hasBlocking(results []AgentResult) bool {
	for _, r := range results {
		if r.Err == nil && r.Verdict != nil && r.Verdict.Status == VerdictBlocking {
			return true
		}
	}
	return false
}

// formatVerdictsForRevision renders non-clean verdicts for the revision prompt.
func formatVerdictsForRevision(results []AgentResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Err != nil || r.Verdict == nil || r.Verdict.Status == VerdictClean {
			continue
		}
		fmt.Fprintf(&b, "### %s critic — %s\n", criticShort(r.Role), r.Verdict.Status)
		for i, f := range r.Verdict.Findings {
			paths := ""
			if len(f.Paths) > 0 {
				paths = " (" + strings.Join(f.Paths, ", ") + ")"
			}
			fmt.Fprintf(&b, "%d. [%s] %s%s\n", i+1, f.Severity, f.Title, paths)
			if f.Detail != "" {
				fmt.Fprintf(&b, "   %s\n", f.Detail)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderReviewNotes produces the "## Review notes" section appended to a design.
// Only the final pass's findings render; revised flags whether a revision
// followed that pass.
func renderReviewNotes(results []AgentResult, round, rounds int, revised bool) string {
	var b strings.Builder
	b.WriteString("## Review notes\n\n")
	fmt.Fprintf(&b, "Reviewed by security, simplicity, and grounding critics (round %d of %d).\n", round, rounds)

	allClean := len(results) > 0
	for _, r := range results {
		if r.Err != nil || r.Verdict == nil || r.Verdict.Status != VerdictClean {
			allClean = false
			break
		}
	}
	if allClean {
		b.WriteString("\nNo findings — the panel signed off on the first draft.\n")
		return b.String()
	}

	b.WriteString("\n")
	for _, r := range results {
		if r.Err != nil || r.Verdict == nil {
			b.WriteString("- _" + criticShort(r.Role) + " critic unavailable (" + unavailableReason(r.Err) + ")_\n")
			continue
		}
		for _, f := range r.Verdict.Findings {
			label := "advisory"
			if f.Severity == VerdictBlocking {
				if revised {
					label = "blocking, addressed in revision"
				} else {
					label = "blocking, unresolved"
				}
			}
			detail := f.Detail
			if len(f.Paths) > 0 {
				if detail != "" {
					detail += " "
				}
				detail += "_(" + strings.Join(f.Paths, ", ") + ")_"
			}
			fmt.Fprintf(&b, "- **%s · %s** — %s\n", criticShort(r.Role), label, f.Title)
			if detail != "" {
				fmt.Fprintf(&b, "  %s\n", detail)
			}
		}
	}
	return b.String()
}

// criticShort maps a critic role to its short display name.
func criticShort(role AgentRole) string {
	switch role {
	case RoleSecCritic:
		return "security"
	case RoleSimpCritic:
		return "simplicity"
	case RoleGrndCritic:
		return "grounding"
	default:
		return string(role)
	}
}

// unavailableReason renders a short reason for a failed/skipped critic.
func unavailableReason(err error) string {
	switch {
	case err == nil:
		return "unavailable"
	case errors.Is(err, errRunBudget):
		return "skipped: run budget exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return firstLine(err.Error())
	}
}

// printCriticLine prints one critic's verdict summary to stderr.
func printCriticLine(role AgentRole, r AgentResult) {
	name := criticShort(role) + " critic"
	if r.Err != nil || r.Verdict == nil {
		warn(name + " — unavailable (" + unavailableReason(r.Err) + ")")
		return
	}
	var blocking, advisory int
	for _, f := range r.Verdict.Findings {
		if f.Severity == VerdictBlocking {
			blocking++
		} else {
			advisory++
		}
	}
	switch r.Verdict.Status {
	case VerdictClean:
		ok(name + " — " + dim("clean"))
	case VerdictBlocking:
		ok(name + " — " + fmt.Sprintf("%d blocking, %d advisory", blocking, advisory))
	default:
		ok(name + " — " + fmt.Sprintf("%d advisory", advisory))
	}
}

// printReviewSummary prints a one-line panel outcome to stderr.
func (s *session) printReviewSummary(results []AgentResult, revised bool) {
	blocking, advisory := 0, 0
	for _, r := range results {
		if r.Err != nil || r.Verdict == nil {
			continue
		}
		for _, f := range r.Verdict.Findings {
			if f.Severity == VerdictBlocking {
				blocking++
			} else {
				advisory++
			}
		}
	}
	switch {
	case blocking == 0 && advisory == 0:
		ok("Review complete — panel signed off")
	case revised:
		ok(fmt.Sprintf("Review complete — %d blocking resolved, %d advisory noted", blocking, advisory))
	default:
		ok(fmt.Sprintf("Review complete — %d blocking, %d advisory", blocking, advisory))
	}
}

// firstLine returns the first line of s, trimmed.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
