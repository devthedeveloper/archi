package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
)

// reviewResult is one critic's contribution to the review document.
type reviewResult struct {
	Role    AgentRole
	Verdict *Verdict
	Skipped string // non-empty reason when the critic didn't run
}

// diffFile is one file's portion of a unified diff.
type diffFile struct {
	path string
	body string
}

// maxReviewChunks caps how many diff chunks are reviewed.
const maxReviewChunks = 8

// Registering here lights up the archi_review MCP tool whenever this file is
// in the build (cmd_serve.go omits the tool while the hook is nil).
func init() { serveReviewCore = reviewCoreForServe }

// reviewCoreForServe runs the critic-panel review non-interactively for the
// archi_review MCP tool: resolve the diff (explicit patch, else git diff
// HEAD), redact, chunk, run the panel, and render the review document.
// blocking mirrors reviewExitCode — the caller treats a blocking verdict as
// content, never as a tool error.
func reviewCoreForServe(ctx context.Context, root, patch string) (doc string, blocking bool, err error) {
	diff, source := patch, "patch"
	if strings.TrimSpace(diff) == "" {
		if diff, err = gitDiffHEAD(root); err != nil {
			return "", false, errors.New("nothing to review — pass a unified diff in patch, or point path at a git repository")
		}
		source = "git diff HEAD"
	}
	if strings.TrimSpace(diff) == "" {
		return "Working tree clean — nothing to review", false, nil
	}
	diff = redactForPrompt("(diff)", diff)

	profile, groundingSkipped := "", ""
	if isInitialized(root) {
		if data, rerr := os.ReadFile(profilePath(root)); rerr == nil {
			profile = string(data)
		}
	} else {
		groundingSkipped = "no .archi cache"
	}
	pName, pModel := "ollama", ""
	if cfg, cerr := loadConfig(root); cerr == nil {
		pName, pModel = cfg.Provider, cfg.Model
	}
	criticMk := func(mt int) Provider { p, _ := newProvider(pName, pModel, 0, mt); return p }

	chunkBudget := contextBudget() - estimateTokens(profile) - 2000
	if chunkBudget < 1000 {
		chunkBudget = 1000
	}
	chunks := packReviewChunks(splitDiffByFile(diff), chunkBudget)
	specs := reviewSpecs()
	if groundingSkipped != "" {
		specs = specs[:2] // sec + simp; grounding needs the profile
	}

	orch := newOrchestrator(root)
	merged := map[AgentRole]*Verdict{}
	for _, chunk := range chunks {
		var chunkText strings.Builder
		for _, f := range chunk {
			chunkText.WriteString(f.body)
		}
		calls := make([]AgentCall, len(specs))
		for i, s := range specs {
			calls[i] = AgentCall{
				Agent: Agent{Spec: s, P: criticMk(s.MaxTokens)},
				Input: TaskInput{Request: "review this diff", Extra: map[string]string{"user_message": reviewUserMessage(profile, chunkText.String())}},
			}
		}
		for i, res := range orch.RunPool(ctx, calls) {
			if res.Err != nil {
				continue
			}
			v, _ := parseVerdict(res.Output)
			if specs[i].Role == RoleGrndCritic {
				downgradePathless(v)
			}
			merged[specs[i].Role] = mergeVerdicts(merged[specs[i].Role], v)
		}
	}

	var results []reviewResult
	for _, role := range criticRoles {
		if role == RoleGrndCritic && groundingSkipped != "" {
			results = append(results, reviewResult{Role: role, Skipped: groundingSkipped})
			continue
		}
		results = append(results, reviewResult{Role: role, Verdict: merged[role]})
	}
	var vs []*Verdict
	for _, r := range results {
		vs = append(vs, r.Verdict)
	}
	return renderReviewDoc(source, results), reviewExitCode(vs) == 1, nil
}

func cmdReview(args []string) {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	patch := fs.String("patch", "", "review this patch file")
	out := fs.String("o", "", "write the review to this file instead of stdout")
	agents := fs.String("agents", "full", "full (critic panel) or fast (one reviewer)")
	criticModel := fs.String("critic-model", "", "cheaper model for reviewers")
	provider := fs.String("provider", "", "override provider")
	model := fs.String("model", "", "override model")
	temp := fs.Float64("temp", 0, "sampling temperature")
	maxTokens := fs.Int("max-tokens", 2000, "max output tokens per critic")
	fs.Usage = reviewUsage
	fs.Parse(args)

	root := "."
	profile, groundingSkipped := "", ""
	if isInitialized(root) {
		if data, err := os.ReadFile(profilePath(root)); err == nil {
			profile = string(data)
		}
	} else {
		groundingSkipped = "no .archi cache"
		warn("no .archi cache — skipping grounding critic; run " + cyan("archi init") + " for repo-aware review")
	}

	diff, source, err := resolveDiff(*patch, os.Stdin, isTTY(os.Stdin), root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  "+red("✗")+" "+err.Error())
		os.Exit(2)
	}
	if strings.TrimSpace(diff) == "" {
		ok("Working tree clean — nothing to review")
		return
	}
	diff = redactForPrompt("(diff)", diff)

	pName, pModel := "ollama", ""
	if isInitialized(root) {
		if cfg, err := loadConfig(root); err == nil {
			pName, pModel = cfg.Provider, cfg.Model
		}
	}
	if *provider != "" {
		pName, pModel = *provider, ""
	}
	if *model != "" {
		pModel = *model
	}
	cm := *criticModel
	if cm == "" {
		cm = pModel
	}
	criticMk := func(mt int) Provider { p, _ := newProvider(pName, cm, *temp, mt); return p }

	files := splitDiffByFile(diff)
	banner()
	info("Reviewing " + source + dim(fmt.Sprintf("  ·  %d files  ·  ~%s tokens", len(files), humanCount(estimateTokens(diff)))))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	orch := newOrchestrator(root)
	chunkBudget := contextBudget() - estimateTokens(profile) - 2000
	if chunkBudget < 1000 {
		chunkBudget = 1000
	}
	chunks := packReviewChunks(files, chunkBudget)

	var specs []AgentSpec
	if *agents == "fast" {
		specs = []AgentSpec{{Role: RoleReviewer, System: reviewerPrompt, MaxTokens: *maxTokens, Temp: *temp}}
	} else {
		specs = reviewSpecs()
		if groundingSkipped != "" {
			specs = specs[:2] // sec + simp; grounding needs the profile
		}
	}

	roles := make([]AgentRole, len(specs))
	for i, s := range specs {
		roles[i] = s.Role
	}
	board := newBoard(roles)
	orch.Board = board

	merged := map[AgentRole]*Verdict{}
	for _, chunk := range chunks {
		var chunkText strings.Builder
		for _, f := range chunk {
			chunkText.WriteString(f.body)
		}
		calls := make([]AgentCall, len(specs))
		for i, s := range specs {
			calls[i] = AgentCall{
				Agent: Agent{Spec: s, P: criticMk(s.MaxTokens)},
				Input: TaskInput{Request: "review this diff", Extra: map[string]string{"user_message": reviewUserMessage(profile, chunkText.String())}},
			}
		}
		results := orch.RunPool(ctx, calls)
		for i, res := range results {
			if res.Err != nil {
				continue
			}
			v, _ := parseVerdict(res.Output)
			if specs[i].Role == RoleGrndCritic {
				downgradePathless(v)
			}
			merged[specs[i].Role] = mergeVerdicts(merged[specs[i].Role], v)
		}
	}
	board.Stop()
	orch.Board = nil

	var results []reviewResult
	if *agents == "fast" {
		results = []reviewResult{{Role: RoleReviewer, Verdict: merged[RoleReviewer]}}
	} else {
		for _, role := range criticRoles {
			if role == RoleGrndCritic && groundingSkipped != "" {
				results = append(results, reviewResult{Role: role, Skipped: groundingSkipped})
				continue
			}
			results = append(results, reviewResult{Role: role, Verdict: merged[role]})
		}
	}

	doc := renderReviewDoc(source, results)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
			failf("%v", err)
		}
		ok("Wrote " + *out)
	} else {
		fmt.Print(doc)
	}

	var vs []*Verdict
	for _, r := range results {
		vs = append(vs, r.Verdict)
	}
	code := reviewExitCode(vs)
	if code == 1 {
		fmt.Fprintln(os.Stderr, "  "+red("✗")+" blocking findings — see the review above")
	} else {
		ok("Review complete")
	}
	os.Exit(code)
}

// mergeVerdicts concatenates findings across chunks, taking the worst status.
func mergeVerdicts(a, b *Verdict) *Verdict {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	a.Findings = append(a.Findings, b.Findings...)
	a.Status = recomputeStatus(a.Findings)
	return a
}

// reviewExitCode maps merged verdicts to the process exit code.
func reviewExitCode(vs []*Verdict) int {
	for _, v := range vs {
		if v != nil && v.Status == VerdictBlocking {
			return 1
		}
	}
	return 0
}

// resolveDiff picks the diff by precedence: patch file, piped stdin, git diff HEAD.
func resolveDiff(patchPath string, stdinR io.Reader, stdinIsTTY bool, root string) (string, string, error) {
	if patchPath != "" {
		data, err := os.ReadFile(patchPath)
		if err != nil {
			return "", "", err
		}
		return string(data), "patch " + patchPath, nil
	}
	if !stdinIsTTY {
		if data, _ := io.ReadAll(stdinR); strings.TrimSpace(string(data)) != "" {
			return string(data), "stdin", nil
		}
	}
	diff, err := gitDiffHEAD(root)
	if err != nil {
		return "", "", errors.New("nothing to review — pass -patch <file>, pipe a diff, or run inside a git repo")
	}
	return diff, "git diff HEAD", nil
}

// gitDiffHEAD returns `git diff HEAD` output; a clean tree is ("", nil), while a
// missing git or non-repo is an error.
func gitDiffHEAD(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "diff", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// splitDiffByFile splits a unified diff on "diff --git " markers. A bare patch
// with no such marker becomes one file taken from its "+++ b/…" header.
func splitDiffByFile(diff string) []diffFile {
	if strings.TrimSpace(diff) == "" {
		return nil
	}
	lines := strings.Split(diff, "\n")
	var files []diffFile
	var cur *diffFile
	var pre []string
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			files = append(files, diffFile{path: gitDiffPath(line), body: line + "\n"})
			cur = &files[len(files)-1]
			continue
		}
		if cur == nil {
			pre = append(pre, line)
			continue
		}
		cur.body += line + "\n"
	}
	if cur == nil && strings.TrimSpace(strings.Join(pre, "\n")) != "" {
		path := "(patch)"
		for _, l := range pre {
			if strings.HasPrefix(l, "+++ b/") {
				path = strings.TrimPrefix(l, "+++ b/")
				break
			}
		}
		return []diffFile{{path: path, body: strings.Join(pre, "\n")}}
	}
	return files
}

// gitDiffPath extracts the b-side path from a "diff --git a/x b/y" line.
func gitDiffPath(line string) string {
	parts := strings.Fields(line)
	if len(parts) >= 4 {
		return strings.TrimPrefix(parts[3], "b/")
	}
	return "(patch)"
}

// packReviewChunks greedily packs diff files into token-bounded chunks. A single
// oversized file is truncated; chunks beyond maxReviewChunks are dropped with a warning.
func packReviewChunks(files []diffFile, budget int) [][]diffFile {
	var chunks [][]diffFile
	var cur []diffFile
	curTokens := 0
	for _, f := range files {
		t := estimateTokens(f.body)
		if t > budget {
			f.body = truncateToTokens(f.body, budget)
			warn("diff for " + f.path + " is large — truncated for review")
			t = estimateTokens(f.body)
		}
		if curTokens+t > budget && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur, curTokens = nil, 0
		}
		cur = append(cur, f)
		curTokens += t
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	if len(chunks) > maxReviewChunks {
		dropped := 0
		for _, c := range chunks[maxReviewChunks:] {
			dropped += len(c)
		}
		warn(fmt.Sprintf("diff too large — skipped %d files beyond %d chunks", dropped, maxReviewChunks))
		chunks = chunks[:maxReviewChunks]
	}
	return chunks
}

// renderReviewDoc builds the review Markdown from per-critic results.
func renderReviewDoc(source string, results []reviewResult) string {
	var b strings.Builder
	b.WriteString("# archi review — " + source + "\n\n")
	b.WriteString("| Critic | Verdict | Findings |\n|---|---|---|\n")
	for _, r := range results {
		name := criticShort(r.Role)
		switch {
		case r.Skipped != "":
			fmt.Fprintf(&b, "| %s | _skipped — %s_ | — |\n", name, r.Skipped)
		case r.Verdict == nil:
			fmt.Fprintf(&b, "| %s | _failed_ | — |\n", name)
		default:
			st := string(r.Verdict.Status)
			if r.Verdict.Status == VerdictBlocking {
				st = "**blocking**"
			}
			fmt.Fprintf(&b, "| %s | %s | %d |\n", name, st, len(r.Verdict.Findings))
		}
	}

	var blocking, advisory []Finding
	for _, r := range results {
		if r.Verdict == nil {
			continue
		}
		for _, f := range r.Verdict.Findings {
			if f.Severity == VerdictBlocking {
				blocking = append(blocking, f)
			} else {
				advisory = append(advisory, f)
			}
		}
	}
	section := func(title string, fs []Finding) {
		if len(fs) == 0 {
			return
		}
		b.WriteString("\n## " + title + "\n\n")
		for i, f := range fs {
			fmt.Fprintf(&b, "%d. **%s** — `%s`\n", i+1, f.Title, strings.Join(f.Paths, ", "))
			if f.Detail != "" {
				b.WriteString("   " + f.Detail + "\n")
			}
		}
	}
	section("Blocking", blocking)
	section("Advisory", advisory)

	exit := 0
	if len(blocking) > 0 {
		exit = 1
	}
	fmt.Fprintf(&b, "\n---\n_%d findings · %d blocking · exit %d_\n", len(blocking)+len(advisory), len(blocking), exit)
	return b.String()
}

func reviewUsage() {
	fmt.Fprint(os.Stderr, `usage: archi review [flags]

  Review a code diff with the critic panel. Exit 0 (clean/advisory), 1 (blocking),
  2 (usage error). Diff source: -patch <file>, piped stdin, or git diff HEAD.

  -patch string      review this patch file
  -o string          write the review to a file instead of stdout
  -agents string     full (critic panel) or fast (one reviewer) (default full)
  -critic-model string  cheaper model for the reviewers
  -provider string   override provider
  -model string      override model
  -max-tokens int    max output tokens per critic (default 2000)
`)
}
