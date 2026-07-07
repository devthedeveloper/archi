package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// jobKind enumerates the work types.
type jobKind string

const (
	jobDesign   jobKind = "design"    // comment-only design
	jobDesignPR jobKind = "design-pr" // design committed + draft PR
	jobBuild    jobKind = "build"     // build on a design PR branch
	jobReview   jobKind = "review"    // explicit /archi review
	jobAutoRev  jobKind = "auto-review"
)

// job is one unit of work, fully self-describing — no webhook payload is
// retained past the handler.
type job struct {
	ID                        string // 8 random bytes, hex
	Kind                      jobKind
	InstallID                 int64
	Repo                      string // "owner/repo"
	DefaultBranch             string
	Issue                     int // issue or PR number the command came from (0 for auto)
	PRNumber                  int // review/build target PR (0 otherwise)
	HeadRef, HeadSHA, BaseRef string
	Fork                      bool
	Request                   string // untrusted text; ≤64 KiB enforced at parse time
	User                      string // requesting login ("" for auto-review)
}

// newJobID returns 8 random bytes as hex.
func newJobID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// errQueueFull is surfaced to the commenter as "archi is busy".
var errQueueFull = errors.New("job queue is full")

// jobQueue is a bounded channel drained by N workers, with a per-repo gate
// so two jobs never share a repo's workspace or cache concurrently.
type jobQueue struct {
	ch      chan job
	gates   map[string]*repoGate
	gatesMu sync.Mutex
	run     func(ctx context.Context, j job)
}

func newJobQueue(size int, run func(ctx context.Context, j job)) *jobQueue {
	return &jobQueue{ch: make(chan job, size), gates: map[string]*repoGate{}, run: run}
}

// enqueue is non-blocking: a full channel returns errQueueFull.
func (q *jobQueue) enqueue(j job) error {
	select {
	case q.ch <- j:
		return nil
	default:
		return errQueueFull
	}
}

// gateFor returns (creating on first use) the repo's gate.
func (q *jobQueue) gateFor(repo string) *repoGate {
	q.gatesMu.Lock()
	defer q.gatesMu.Unlock()
	g, ok := q.gates[repo]
	if !ok {
		g = &repoGate{}
		q.gates[repo] = g
	}
	return g
}

// start launches n workers. Each holds the job's repo mutex for the whole
// run, so a repo's jobs serialize while different repos proceed in parallel.
func (q *jobQueue) start(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-q.ch:
					g := q.gateFor(j.Repo)
					g.mu.Lock()
					q.run(ctx, j)
					g.mu.Unlock()
				}
			}
		}()
	}
}

// repoGate serializes a repo's jobs (mu, held per job) and rate-limits its
// enqueues (rateMu + starts — separate so a running job never blocks the
// webhook path's rate check).
type repoGate struct {
	mu     sync.Mutex
	rateMu sync.Mutex
	starts []time.Time // pruned sliding window
}

// allow records a start and reports whether the repo is under maxPerHour.
func (g *repoGate) allow(now time.Time, maxPerHour int) bool {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	cutoff := now.Add(-time.Hour)
	kept := g.starts[:0]
	for _, t := range g.starts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	g.starts = kept
	if len(g.starts) >= maxPerHour {
		return false
	}
	g.starts = append(g.starts, now)
	return true
}

// ---- the pipeline (§6): clone → cache/init → run CLI → post results ----

// runJob executes one job end to end. It is called with the repo's mutex
// held and never panics the worker: every failure lands as a comment, a
// neutral check run, or (when there is nowhere left to post) a log line.
func (s *server) runJob(ctx context.Context, j job) {
	start := time.Now()
	logf("info", "job started", "job", j.ID, "kind", string(j.Kind), "repo", j.Repo)
	defer func() {
		if r := recover(); r != nil {
			logf("error", "job panicked", "job", j.ID, "err", fmt.Sprint(r))
		}
		logf("info", "job finished", "job", j.ID, "kind", string(j.Kind), "repo", j.Repo,
			"took", time.Since(start).Round(time.Second).String())
	}()

	token, err := s.auth.installationToken(ctx, j.InstallID)
	if err != nil {
		logf("error", "installation token", "job", j.ID, "err", err.Error())
		return
	}
	gh := &ghClient{base: s.apiBase, token: token, http: s.httpc}
	cfg, warns := repoConfigFor(ctx, gh, j.Repo, j.DefaultBranch)

	isReview := j.Kind == jobReview || j.Kind == jobAutoRev
	if isReview {
		// The in_progress check run is the ack (and, for auto-review, the
		// only one — there is no comment to react to).
		if err := gh.createCheckRun(ctx, j.Repo, checkRun{
			Name: checkName, HeadSHA: j.HeadSHA, Status: "in_progress",
			Output: checkOutput{Title: "archi review running", Summary: "Reviewing this PR's diff…"},
		}); err != nil {
			logf("warn", "check run create failed", "job", j.ID, "err", err.Error())
		}
	}

	// fail posts the failure where the requester will see it: a comment for
	// comment-driven jobs, a neutral check run for reviews.
	fail := func(msg string) {
		logf("error", "job failed", "job", j.ID, "kind", string(j.Kind), "repo", j.Repo, "msg", msg)
		if isReview {
			gh.createCheckRun(ctx, j.Repo, checkRun{
				Name: checkName, HeadSHA: j.HeadSHA, Status: "completed", Conclusion: "neutral",
				Output: checkOutput{Title: "archi review could not run", Summary: msg},
			})
			if j.Kind == jobAutoRev {
				return
			}
		}
		if j.Issue > 0 {
			gh.postComment(ctx, j.Repo, j.Issue, "archi "+string(j.Kind)+" failed: "+msg)
		}
	}

	// Checkout ref: default branch for designs, PR head for builds, PR BASE
	// for reviews — fork code is never checked out (§security).
	ref := j.DefaultBranch
	switch j.Kind {
	case jobBuild:
		ref = j.HeadRef
	case jobReview, jobAutoRev:
		ref = j.BaseRef
	}

	jctx, cancel := context.WithTimeout(ctx, s.env.JobTimeout)
	defer cancel()

	ws, err := prepare(jctx, j, ref, token, s.env)
	if err != nil {
		fail(fmt.Sprintf("could not clone %s at %s: %v", j.Repo, ref, err))
		return
	}
	defer ws.cleanup()

	if err := s.ensureCache(jctx, j, ws, cfg, ref); err != nil {
		if jctx.Err() != nil {
			fail(s.timeoutMsg())
			return
		}
		fail("archi couldn't learn this repo: " + err.Error())
		return
	}

	switch j.Kind {
	case jobDesign, jobDesignPR:
		s.runDesignJob(jctx, gh, j, ws, cfg, warns, fail)
	case jobBuild:
		s.runBuildJob(jctx, gh, j, ws, cfg, warns, fail)
	case jobReview, jobAutoRev:
		s.runReviewJob(jctx, gh, j, ws, cfg, warns, fail)
	}
}

// timeoutMsg names the limit and the escape hatches.
func (s *server) timeoutMsg() string {
	return fmt.Sprintf("timed out after %s — try `agents: fast` in .github/archi.yml or a smaller request", s.env.JobTimeout)
}

// ensureCache restores the shared .archi cache and brings it up to date:
// full init when absent, incremental refresh when the recorded SHA moved,
// nothing when it matches. A failed refresh falls back to one full init.
// The cache is saved back only for default-branch runs — PR-branch profiles
// would poison the shared understanding.
func (s *server) ensureCache(ctx context.Context, j job, ws *workspace, cfg repoConfig, ref string) error {
	cached, _ := restoreCache(s.env.CacheDir, j, ws)
	head, err := ws.runGit(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	head = strings.TrimSpace(head)

	base := []string{"init"}
	if cfg.Provider != "" {
		base = append(base, "-provider", cfg.Provider)
	}
	if cfg.Model != "" {
		base = append(base, "-model", cfg.Model)
	}
	initFull := func() error {
		os.RemoveAll(filepath.Join(ws.dir, ".archi"))
		_, stderr, exit, err := ws.runArchi(ctx, s.env, base...)
		if err != nil {
			return err
		}
		if exit != 0 {
			return errors.New(tail(stderr, 12))
		}
		return nil
	}

	ran := false
	switch {
	case cached == "":
		if err := initFull(); err != nil {
			return err
		}
		ran = true
	case cached != head:
		refresh := append([]string{"init", "-refresh"}, base[1:]...)
		_, stderr, exit, err := ws.runArchi(ctx, s.env, refresh...)
		if err != nil || exit != 0 {
			logf("warn", "refresh failed — falling back to full init", "job", j.ID, "detail", tail(stderr, 4))
			if err := initFull(); err != nil {
				return err
			}
		}
		ran = true
	}
	if ran && ref == j.DefaultBranch {
		if err := saveCache(s.env.CacheDir, j, ws, head); err != nil {
			logf("warn", "cache save failed", "job", j.ID, "err", err.Error())
		}
	}
	return nil
}

// designArgs assembles the CLI flags a design/build invocation shares.
func designFlags(cfg repoConfig) []string {
	var args []string
	if cfg.Provider != "" {
		args = append(args, "-provider", cfg.Provider)
	}
	if cfg.Model != "" {
		args = append(args, "-model", cfg.Model)
	}
	if cfg.Agents != "" {
		args = append(args, "-agents", cfg.Agents)
	}
	if cfg.CriticModel != "" {
		args = append(args, "-critic-model", cfg.CriticModel)
	}
	if cfg.MaxTokens > 0 {
		args = append(args, "-max-tokens", fmt.Sprint(cfg.MaxTokens))
	}
	return args
}

// warnFootnote renders repo-config warnings once per job.
func warnFootnote(warns []string) string {
	if len(warns) == 0 {
		return ""
	}
	return "\n\n---\n_config notes: " + strings.Join(warns, "; ") + "_"
}

// postParts posts a (possibly split) comment thread-safely in order.
func postParts(ctx context.Context, gh *ghClient, repo string, issue int, body string) error {
	for _, part := range splitComment(body, commentMax) {
		if _, err := gh.postComment(ctx, repo, issue, part); err != nil {
			return err
		}
	}
	return nil
}

// runDesignJob runs `archi design -no-interactive` and posts the document —
// as comments for jobDesign, as a committed design doc + draft PR for
// jobDesignPR.
func (s *server) runDesignJob(ctx context.Context, gh *ghClient, j job, ws *workspace, cfg repoConfig, warns []string, fail func(string)) {
	args := append([]string{"design", "-no-interactive"}, designFlags(cfg)...)
	args = append(args, j.Request) // the request is ONE argv element
	stdout, stderr, exit, err := ws.runArchi(ctx, s.env, args...)
	if ctx.Err() != nil {
		fail(s.timeoutMsg())
		return
	}
	if err != nil {
		fail(err.Error())
		return
	}
	if exit != 0 {
		fail("the design run failed:\n\n```\n" + tail(stderr, 12) + "\n```")
		return
	}
	doc := strings.TrimSpace(stdout)
	if doc == "" {
		fail("the design run produced no output")
		return
	}

	if j.Kind == jobDesign {
		if err := postParts(ctx, gh, j.Repo, j.Issue, doc+warnFootnote(warns)); err != nil {
			logf("error", "posting design", "job", j.ID, "err", err.Error())
		}
		return
	}

	// jobDesignPR: commit the doc and open a draft PR.
	slug := slugify(j.Request)
	relPath := "docs/designs/" + slug + ".md"
	full := filepath.Join(ws.dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		fail(err.Error())
		return
	}
	if err := os.WriteFile(full, []byte(doc+"\n"), 0o644); err != nil {
		fail(err.Error())
		return
	}
	branch := fmt.Sprintf("archi/design-%d-%s", j.Issue, slug)
	if err := ws.commitPush(ctx, branch, fmt.Sprintf("archi: design for #%d", j.Issue), "docs/designs"); err != nil {
		fail(err.Error())
		return
	}
	title := "design: " + clip(oneLine(j.Request), 70)
	prBody := fmt.Sprintf("Design for #%d, generated by archi from:\n\n> %s\n\nReview `%s`, then comment `/archi build` on this PR to generate the code.",
		j.Issue, clip(oneLine(j.Request), 400), relPath)
	_, url, err := gh.createPR(ctx, j.Repo, title, branch, j.DefaultBranch, prBody, true)
	if err != nil {
		fail("design pushed to `" + branch + "` but opening the PR failed: " + err.Error())
		return
	}
	msg := fmt.Sprintf("Design ready → %s. Review `%s`, then comment `/archi build` on the PR.", url, relPath)
	if err := postParts(ctx, gh, j.Repo, j.Issue, msg+warnFootnote(warns)); err != nil {
		logf("error", "posting design-pr comment", "job", j.ID, "err", err.Error())
	}
}

// runBuildJob turns the committed design doc on an archi design PR branch
// into code on the SAME branch.
func (s *server) runBuildJob(ctx context.Context, gh *ghClient, j job, ws *workspace, cfg repoConfig, warns []string, fail func(string)) {
	docPath, err := findDesignDoc(ws.dir, j.HeadRef)
	if err != nil {
		fail("this isn't an archi design PR — start with `/archi design --pr <request>` on an issue. (" + err.Error() + ")")
		return
	}
	design, err := os.ReadFile(docPath)
	if err != nil {
		fail(err.Error())
		return
	}
	// The request file lives OUTSIDE the clone so `git add -A` can't commit it.
	reqFile := filepath.Join(filepath.Dir(ws.dir), j.ID+"-request.md")
	preamble := "Implement exactly this approved design; do not redesign.\n\n"
	if err := os.WriteFile(reqFile, []byte(preamble+string(design)), 0o644); err != nil {
		fail(err.Error())
		return
	}
	defer os.Remove(reqFile)

	args := append([]string{"design", "-no-interactive", "-build", "-yes", "-f", reqFile}, designFlags(cfg)...)
	_, stderr, exit, err := ws.runArchi(ctx, s.env, args...)
	if ctx.Err() != nil {
		fail(s.timeoutMsg())
		return
	}
	if err != nil {
		fail(err.Error())
		return
	}
	if exit != 0 {
		fail("the build run failed:\n\n```\n" + tail(stderr, 12) + "\n```")
		return
	}

	status, err := ws.runGit(ctx, "status", "--porcelain")
	if err != nil {
		fail(err.Error())
		return
	}
	if strings.TrimSpace(status) == "" {
		fail("the build produced no file changes — the design may need a concrete file-by-file plan")
		return
	}
	table := changeTable(status)
	if _, err := ws.runGit(ctx, "add", "-A", "."); err != nil {
		fail(err.Error())
		return
	}
	stat, _ := ws.runGit(ctx, "diff", "--cached", "--stat")
	if err := ws.commitPush(ctx, j.HeadRef, "archi: build from approved design", "."); err != nil {
		if strings.Contains(err.Error(), "branch moved") {
			fail("branch moved while I was building — re-run `/archi build`")
			return
		}
		fail(err.Error())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Built from the approved design on `%s`:\n\n%s", j.HeadRef, table)
	if strings.TrimSpace(stat) != "" {
		fmt.Fprintf(&b, "\n```\n%s```\n", stat)
	}
	if warnTail := tail(stderr, 6); warnTail != "" {
		fmt.Fprintf(&b, "\nCLI output tail:\n\n```\n%s\n```", warnTail)
	}
	if err := postParts(ctx, gh, j.Repo, j.Issue, b.String()+warnFootnote(warns)); err != nil {
		logf("error", "posting build result", "job", j.ID, "err", err.Error())
	}
}

// runReviewJob reviews the PR's API-fetched diff on a BASE-ref workspace and
// publishes a PR review plus the "archi review" check run.
func (s *server) runReviewJob(ctx context.Context, gh *ghClient, j job, ws *workspace, cfg repoConfig, warns []string, fail func(string)) {
	diff, err := gh.prDiff(ctx, j.Repo, j.PRNumber)
	if err != nil {
		fail(err.Error())
		return
	}
	if strings.TrimSpace(diff) == "" {
		gh.createCheckRun(ctx, j.Repo, checkRun{
			Name: checkName, HeadSHA: j.HeadSHA, Status: "completed", Conclusion: "success",
			Output: checkOutput{Title: "archi review: empty diff", Summary: "Nothing to review."},
		})
		return
	}
	if err := os.WriteFile(filepath.Join(ws.dir, "pr.diff"), []byte(diff), 0o644); err != nil {
		fail(err.Error())
		return
	}

	args := []string{"review", "-patch", "pr.diff"}
	if cfg.Agents != "" {
		args = append(args, "-agents", cfg.Agents)
	}
	if cfg.CriticModel != "" {
		args = append(args, "-critic-model", cfg.CriticModel)
	}
	if cfg.Provider != "" {
		args = append(args, "-provider", cfg.Provider)
	}
	if cfg.Model != "" {
		args = append(args, "-model", cfg.Model)
	}
	stdout, stderr, exit, err := ws.runArchi(ctx, s.env, args...)
	if ctx.Err() != nil {
		fail(s.timeoutMsg())
		return
	}
	ranOK := err == nil && (exit == 0 || exit == 1) && strings.TrimSpace(stdout) != ""
	outcome := parseReviewFooter(stdout, exit)
	conclusion, title := conclusionFor(outcome, ranOK)

	if !ranOK {
		detail := tail(stderr, 12)
		if err != nil {
			detail = err.Error()
		}
		fail("archi review could not run:\n\n```\n" + detail + "\n```")
		return
	}

	doc := strings.TrimSpace(stdout) + warnFootnote(warns)
	if len(doc) <= commentMax {
		if err := gh.createReview(ctx, j.Repo, j.PRNumber, doc); err != nil {
			logf("error", "posting review", "job", j.ID, "err", err.Error())
		}
	} else {
		gh.createReview(ctx, j.Repo, j.PRNumber, "The review is too long for one review body — posted as comments below.")
		if err := postParts(ctx, gh, j.Repo, j.PRNumber, doc); err != nil {
			logf("error", "posting review parts", "job", j.ID, "err", err.Error())
		}
	}
	if err := gh.createCheckRun(ctx, j.Repo, checkRun{
		Name: checkName, HeadSHA: j.HeadSHA, Status: "completed", Conclusion: conclusion,
		Output: checkOutput{
			Title:   title,
			Summary: fmt.Sprintf("%d findings · %d blocking", outcome.Findings, outcome.Blocking),
			Text:    truncateCheckText(outcome.Doc),
		},
	}); err != nil {
		logf("error", "completing check run", "job", j.ID, "err", err.Error())
	}
}

// findDesignDoc locates the committed design under docs/designs/, preferring
// a file whose slug appears in the branch name.
func findDesignDoc(dir, branch string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "docs", "designs", "*.md"))
	if err != nil || len(matches) == 0 {
		return "", errors.New("no design doc under docs/designs/")
	}
	sort.Strings(matches)
	for _, m := range matches {
		slug := strings.TrimSuffix(filepath.Base(m), ".md")
		if strings.Contains(branch, slug) {
			return m, nil
		}
	}
	return matches[0], nil
}

// changeTable renders `git status --porcelain` as the result comment's table.
func changeTable(porcelain string) string {
	var b strings.Builder
	b.WriteString("| change | path |\n|---|---|\n")
	for _, line := range strings.Split(strings.TrimSpace(porcelain), "\n") {
		if len(line) < 4 {
			continue
		}
		code, path := strings.TrimSpace(line[:2]), strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		kind := "overwrite"
		switch {
		case strings.Contains(code, "?") || strings.Contains(code, "A"):
			kind = "create"
		case strings.Contains(code, "D"):
			kind = "delete"
		case strings.Contains(code, "R"):
			kind = "rename"
		}
		fmt.Fprintf(&b, "| %s | %s |\n", kind, path)
	}
	return b.String()
}

// slugify mirrors the CLI's history slug: lowercase, runs outside [a-z0-9]
// collapse to single dashes, capped at 40 chars, "design" when empty.
func slugify(request string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(request) {
		if b.Len() >= 40 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "design"
	}
	return s
}

// oneLine collapses whitespace runs so a request reads as a title.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip truncates s to n bytes with an ellipsis.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// tail returns the last n non-empty lines of s, trimmed.
func tail(s string, n int) string {
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}
