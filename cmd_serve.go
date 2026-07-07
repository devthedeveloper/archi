package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// `archi serve` exposes archi as an MCP server (see mcp.go for the protocol
// layer). Each tool is a thin bridge onto an existing non-interactive flow:
// statusCore, initCore, session.designDoc, previewChanges/writeChanges, and
// the Phase 3/4 hooks below. Handlers never call failf, confirm, or anything
// that writes to stdout — stdout belongs to the protocol.

// serveReviewCore is nil until the review pipeline registers itself from
// cmd_review.go's init(); while nil the archi_review tool is omitted from
// tools/list and calling it fails like any unknown tool.
var serveReviewCore func(ctx context.Context, root, patch string) (doc string, blocking bool, err error)

// serveRollbackCore is nil until the rollback pipeline registers itself from
// cmd_rollback.go's init(); gated exactly like serveReviewCore.
var serveRollbackCore func(root, run string, list bool) (doc string, err error)

// cmdServe runs the MCP server on stdin/stdout until EOF.
func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	root := fs.String("root", ".", "confine every tool call to this directory")
	fs.Usage = serveUsage
	fs.Parse(args)

	s, err := newMCPServer(*root)
	if err != nil {
		failf("%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.serve(ctx, os.Stdin, os.Stdout); err != nil {
		failf("%v", err)
	}
}

func serveUsage() {
	fmt.Fprint(os.Stderr, `usage: archi serve [flags]

  Run archi as a Model Context Protocol (MCP) server over stdio, exposing
  status/init/design/build/review/rollback as tools for agent clients
  (Claude Code, Claude Desktop, Cursor, ...). stdout carries protocol JSON
  only; progress and logging go to stderr. Provider API keys come from this
  process's environment; set ARCHI_TIMEOUT (e.g. "10m") to cap each call.

  -root string   confine every tool call to this directory (default ".")

  Register with Claude Code:
    claude mcp add archi -- archi serve -root "$(pwd)"
`)
}

// newMCPServer builds the server with the standard tool set; review/rollback
// register only when their hooks are non-nil.
func newMCPServer(root string) (*mcpServer, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("-root %s: %v", root, err)
	}
	if fi, err := os.Stat(resolved); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("-root %s is not a directory", root)
	}
	s := &mcpServer{root: resolved, protocol: knownProtocolVersions[0], mk: newProvider}
	s.tools = []toolDef{
		{Name: "archi_status", Description: statusToolDesc, InputSchema: json.RawMessage(statusSchema), handler: s.toolStatus},
		{Name: "archi_init", Description: initToolDesc, InputSchema: json.RawMessage(initSchema), handler: s.toolInit},
		{Name: "archi_design", Description: designToolDesc, InputSchema: json.RawMessage(designSchema), handler: s.toolDesign},
		{Name: "archi_build", Description: buildToolDesc, InputSchema: json.RawMessage(buildSchema), handler: s.toolBuild},
	}
	if serveReviewCore != nil {
		s.tools = append(s.tools, toolDef{Name: "archi_review", Description: reviewToolDesc, InputSchema: json.RawMessage(reviewSchema), handler: s.toolReview})
	}
	if serveRollbackCore != nil {
		s.tools = append(s.tools, toolDef{Name: "archi_rollback", Description: rollbackToolDesc, InputSchema: json.RawMessage(rollbackSchema), handler: s.toolRollback})
	}
	return s, nil
}

// Tool descriptions are prompt engineering for the calling LLM, not comments;
// keep them in sync with the schemas below.
const (
	statusToolDesc = "Show what archi has learned about a repository: provider/model, when it was " +
		"initialized, file and token counts, whether the cache is stale relative to the " +
		"working tree, top languages, and the five most recent saved designs. Call this " +
		"first to check whether `archi_init` is needed before designing."

	initToolDesc = "Learn or refresh archi's cached understanding of a repository (writes " +
		"`.archi/`). Run once per repo before designing, and with `refresh:true` when " +
		"the status tool reports drift. Slow: it scans the repo and calls the " +
		"configured LLM to build a codebase profile. Refresh updates incrementally; " +
		"force rebuilds from scratch. `refresh` and `force` are mutually exclusive."

	designToolDesc = "Produce a design document (briefing, Mermaid diagrams, file-by-file plan) " +
		"for a change request, grounded in the learned codebase. Returns the design " +
		"as Markdown; it is also saved under `.archi/designs/`. Non-interactive: there " +
		"is no clarifying interview, so put every constraint, preference, and answer " +
		"you already have directly into `request`. Requires `archi_init` first. " +
		"`agents:\"full\"` runs the multi-agent ensemble (explorer + designer + critics); " +
		"`\"fast\"` is a single model call."

	buildToolDesc = "Turn an archi design document into concrete file changes. By default this is " +
		"a DRY RUN: it returns the proposed create/overwrite/delete list with unified " +
		"diffs and writes nothing. Set `dry_run:false` to actually write the files " +
		"(originals are backed up to `.archi/backups/<stamp>/` for rollback). Writing " +
		"is refused while the repo has uncommitted changes unless `allow_dirty:true`. " +
		"Provide the design either inline (`design`) or as a saved file path " +
		"(`design_file`) — exactly one."

	reviewToolDesc = "Review a patch with archi's critic panel (security, simplicity, grounding). " +
		"Pass a unified diff in `patch`, or omit it to review the repo's uncommitted " +
		"work (`git diff HEAD`). Returns a Markdown review with per-critic verdicts " +
		"and findings."

	rollbackToolDesc = "Undo an archi build: restore the files a previous apply overwrote or deleted " +
		"and remove the files it created, from `.archi/backups/`. With `list:true`, " +
		"just list available backup runs. `run` selects a specific run stamp " +
		"(e.g. \"20260706-142317\"); omitted means the newest."
)

const statusSchema = `{"type":"object","additionalProperties":false,
 "properties":{"path":{"type":"string","description":"Repo path relative to the server root; omit for the root itself."}}}`

const initSchema = `{"type":"object","additionalProperties":false,
 "properties":{
   "path":{"type":"string","description":"Repo path relative to the server root."},
   "refresh":{"type":"boolean","default":false,"description":"Incrementally update an existing cache from what changed."},
   "force":{"type":"boolean","default":false,"description":"Rebuild even if .archi already exists."}}}`

const designSchema = `{"type":"object","additionalProperties":false,"required":["request"],
 "properties":{
   "request":{"type":"string","minLength":1,"description":"The design request, with all known constraints and clarifications inlined."},
   "path":{"type":"string","description":"Repo path relative to the server root."},
   "focus":{"type":"array","items":{"type":"string"},"description":"Globs of files to include as extra grounding, e.g. [\"internal/pay/**\"]."},
   "agents":{"type":"string","enum":["full","fast"],"default":"full","description":"full = design ensemble, fast = single model call."},
   "max_tokens":{"type":"integer","minimum":256,"maximum":64000,"default":8000,"description":"Max output tokens for the design."}}}`

const buildSchema = `{"type":"object","additionalProperties":false,
 "properties":{
   "design":{"type":"string","description":"The design document Markdown, inline."},
   "design_file":{"type":"string","description":"Path (relative to the server root) of a saved design, e.g. .archi/designs/20260706-...md."},
   "path":{"type":"string","description":"Repo path relative to the server root."},
   "dry_run":{"type":"boolean","default":true,"description":"true = report proposed changes and diffs only; false = write files with backups."},
   "allow_dirty":{"type":"boolean","default":false,"description":"Permit writing even when the repo has uncommitted changes."}}}`

const reviewSchema = `{"type":"object","additionalProperties":false,
 "properties":{
   "patch":{"type":"string","description":"Unified diff text to review. Omit to review git diff HEAD in the repo."},
   "path":{"type":"string","description":"Repo path relative to the server root."}}}`

const rollbackSchema = `{"type":"object","additionalProperties":false,
 "properties":{
   "path":{"type":"string","description":"Repo path relative to the server root."},
   "run":{"type":"string","pattern":"^[0-9]{8}-[0-9]{6}$","description":"Backup run stamp to restore; omit for the newest."},
   "list":{"type":"boolean","default":false,"description":"List available backup runs instead of restoring."}}}`

// resolveToolPath confines a tool-supplied path inside the server root:
// ""/"." ⇒ root; otherwise it must pass safeRel (relative, no "..") and the
// join must still be under root after resolving symlinks on any existing
// prefix — so a link inside the root can't point the path outside it.
// Returns the absolute directory/file path.
func resolveToolPath(root, p string) (string, error) {
	if p == "" || p == "." {
		return root, nil
	}
	rel, ok := safeRel(p)
	if !ok {
		return "", fmt.Errorf("path %q escapes the server root — use a relative path inside it", p)
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	resolved, err := resolveExistingPrefix(full)
	if err != nil {
		return "", err
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the server root — use a relative path inside it", p)
	}
	return full, nil
}

// resolveExistingPrefix runs filepath.EvalSymlinks on the longest existing
// prefix of path and rejoins the (not yet existing) remainder.
func resolveExistingPrefix(path string) (string, error) {
	suffix := ""
	cur := path
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, suffix), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path, nil
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

// decodeArgs strict-decodes arguments into dst: json.Decoder with
// DisallowUnknownFields over the raw bytes; empty/null args decode to zero.
// Any error is surfaced by the caller as JSON-RPC -32602.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// decodeToolArgs wraps decodeArgs failures in the -32602 sentinel.
func decodeToolArgs(raw json.RawMessage, dst any) error {
	if err := decodeArgs(raw, dst); err != nil {
		return &errInvalidArgs{detail: err.Error()}
	}
	return nil
}

// Per-tool argument structs mirror the schemas field-for-field. shape()
// methods enforce the constraints strict decoding can't express (required,
// enum, range, pattern) — those are -32602 rejections, while semantic
// conflicts like refresh+force are handler errors (tool isError).

type statusArgs struct {
	Path string `json:"path"`
}

type initArgs struct {
	Path    string `json:"path"`
	Refresh bool   `json:"refresh"`
	Force   bool   `json:"force"`
}

type designArgs struct {
	Request   string   `json:"request"`
	Path      string   `json:"path"`
	Focus     []string `json:"focus"`
	Agents    string   `json:"agents"`
	MaxTokens *int     `json:"max_tokens"` // pointer: absent defaults to 8000, explicit 0 is a range violation
}

func (a *designArgs) shape() error {
	if a.Request == "" {
		return errors.New(`missing required "request"`)
	}
	switch a.Agents {
	case "", "full", "fast":
	default:
		return fmt.Errorf(`"agents" must be one of full|fast, got %q`, a.Agents)
	}
	if a.MaxTokens != nil && (*a.MaxTokens < 256 || *a.MaxTokens > 64000) {
		return fmt.Errorf(`"max_tokens" must be between 256 and 64000, got %d`, *a.MaxTokens)
	}
	return nil
}

type buildArgs struct {
	Design     string `json:"design"`
	DesignFile string `json:"design_file"`
	Path       string `json:"path"`
	DryRun     *bool  `json:"dry_run"` // pointer: the default is true, the safe call
	AllowDirty bool   `json:"allow_dirty"`
}

type reviewArgs struct {
	Patch string `json:"patch"`
	Path  string `json:"path"`
}

var rollbackStampRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

type rollbackArgs struct {
	Path string `json:"path"`
	Run  string `json:"run"`
	List bool   `json:"list"`
}

func (a *rollbackArgs) shape() error {
	if a.Run != "" && !rollbackStampRe.MatchString(a.Run) {
		return fmt.Errorf(`"run" must match ^[0-9]{8}-[0-9]{6}$, got %q`, a.Run)
	}
	return nil
}

// toolStatus implements archi_status: a Markdown snapshot of statusCore.
func (s *mcpServer) toolStatus(ctx context.Context, raw json.RawMessage, _ func(string, float64)) (string, error) {
	var a statusArgs
	if err := decodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	dir, err := resolveToolPath(s.root, a.Path)
	if err != nil {
		return "", err
	}
	if !isInitialized(dir) {
		return "", errors.New("not initialized — call archi_init first")
	}
	rep, err := statusCore(dir)
	if err != nil {
		return "", err
	}
	return renderStatusMarkdown(rep), nil
}

// renderStatusMarkdown renders a statusReport for an LLM caller.
func renderStatusMarkdown(rep *statusReport) string {
	var b strings.Builder
	b.WriteString("# archi status\n\n")
	fmt.Fprintf(&b, "- **provider:** %s/%s\n", rep.Cfg.Provider, rep.Cfg.Model)
	fmt.Fprintf(&b, "- **initialized:** %s\n", rep.Cfg.InitializedAt)
	fmt.Fprintf(&b, "- **files:** %s (~%s tokens)\n", humanCount(rep.Cfg.FileCount), humanCount(rep.Cfg.ApproxTokens))
	if rep.Fresh {
		v := "fresh"
		if !rep.Drift.empty() {
			v = rep.Drift.summary() + " — call archi_init with refresh:true"
		}
		if rep.Age > 0 {
			v += " (cached " + humanAge(rep.Age) + " ago)"
		}
		fmt.Fprintf(&b, "- **freshness:** %s\n", v)
	}
	if len(rep.Stats) > 0 {
		b.WriteString("\n## Top languages\n\n")
		for i, st := range rep.Stats {
			if i >= 6 {
				break
			}
			fmt.Fprintf(&b, "- %s — ~%s tokens\n", st.name, humanCount(st.tokens))
		}
	}
	if len(rep.Recent) > 0 {
		b.WriteString("\n## Recent designs\n\n| stamp | design | state |\n|---|---|---|\n")
		for _, r := range rep.Recent {
			state := "draft"
			if r.built {
				state = "built"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", r.stamp, r.slug, state)
		}
	}
	return b.String()
}

// toolInit implements archi_init over initCore, reporting coarse progress
// milestones when the client sent a progressToken.
func (s *mcpServer) toolInit(ctx context.Context, raw json.RawMessage, progress func(string, float64)) (string, error) {
	var a initArgs
	if err := decodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	dir, err := resolveToolPath(s.root, a.Path)
	if err != nil {
		return "", err
	}
	if a.Refresh && a.Force {
		return "", errors.New("refresh and force are mutually exclusive — refresh updates, force rebuilds")
	}
	if a.Refresh && !isInitialized(dir) {
		return "", errors.New("nothing to refresh — no .archi cache here; call archi_init without refresh first")
	}
	if isInitialized(dir) && !a.Force && !a.Refresh {
		return "", errors.New("already initialized — pass refresh:true to update the cache or force:true to rebuild it")
	}

	// No per-call overrides: reuse the backend that built the cache, else the default.
	provider, model := "ollama", ""
	if cfg, err := loadConfig(dir); err == nil {
		provider, model = cfg.Provider, cfg.Model
	}
	rep, err := initCore(ctx, initOptions{
		Root: dir, Provider: provider, Model: model,
		Force: a.Force, Refresh: a.Refresh,
		MaxFileBytes: 256 * 1024,
		Progress:     progress,
		mk:           s.mk,
	})
	if err != nil {
		return "", err
	}
	backend := provider
	if cfg, err := loadConfig(dir); err == nil {
		backend = cfg.Provider + "/" + cfg.Model
	}
	switch {
	case rep.Fresh:
		return "Cache was already fresh — nothing to refresh.", nil
	case rep.Refreshed:
		return fmt.Sprintf("Refreshed from %s with %s.", rep.Drift, backend), nil
	case a.Refresh && rep.Drift != "":
		return fmt.Sprintf("Drift too large (%s) — rebuilt: learned %s files (~%s tokens) with %s.",
			rep.Drift, humanCount(rep.Files), humanCount(rep.Tokens), backend), nil
	default:
		return fmt.Sprintf("Learned %s files (~%s tokens) with %s.",
			humanCount(rep.Files), humanCount(rep.Tokens), backend), nil
	}
}

// toolDesign implements archi_design: one non-interactive design pass over
// the cached grounding, saved to .archi/designs like every CLI design.
func (s *mcpServer) toolDesign(ctx context.Context, raw json.RawMessage, _ func(string, float64)) (string, error) {
	var a designArgs
	if err := decodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	if err := a.shape(); err != nil {
		return "", &errInvalidArgs{detail: err.Error()}
	}
	dir, err := resolveToolPath(s.root, a.Path)
	if err != nil {
		return "", err
	}
	g, err := loadGrounding(dir, a.Focus)
	if err != nil {
		return "", err
	}
	maxTokens := 8000
	if a.MaxTokens != nil {
		maxTokens = *a.MaxTokens
	}
	mode := a.Agents
	if mode == "" {
		mode = "full"
	}

	// The backend is the one the cache was built with — keys never cross the wire.
	if _, err := s.mk(g.cfg.Provider, g.cfg.Model, 0.4, maxTokens); err != nil {
		return "", err
	}
	mk := func(mt int) Provider { p, _ := s.mk(g.cfg.Provider, g.cfg.Model, 0.4, mt); return p }
	criticModel := g.cfg.CriticModel
	if criticModel == "" {
		criticModel = g.cfg.Model
	}
	criticMk := func(mt int) Provider { p, _ := s.mk(g.cfg.Provider, criticModel, 0, mt); return p }
	rounds := g.cfg.Rounds
	if rounds < 1 {
		rounds = 1
	}
	if rounds > 3 {
		rounds = 3
	}

	sess := &session{
		ctx: ctx, mk: mk, root: dir, cfg: g.cfg,
		profile: g.profile, fileMap: g.fileMap, focus: g.focus,
		maxTokens: maxTokens,
		opts:      ensembleOpts{mode: mode, rounds: rounds, criticModel: g.cfg.CriticModel},
		criticMk:  criticMk,
		request:   a.Request,
		memory:    fitMemory(loadMemory(dir), memoryBudget()),
	}
	sess.orch = newOrchestrator(dir)
	doc, err := sess.designDoc(a.Request)
	if err != nil {
		return "", err
	}
	out := doc
	if sess.historyPath != "" { // history is advisory; omit the note if it failed
		out += toolTextSep + "Saved to " + archiDirName + "/designs/" + filepath.Base(sess.historyPath)
	}
	return out, nil
}

// maxToolDiffLines caps each file's diff in an archi_build dry-run result.
const maxToolDiffLines = 400

// toolBuild implements archi_build: generate the file changes for a design,
// then either report them (dry run, the default) or write them with backups.
// It can never reach a confirm prompt — the flags are the confirmation.
func (s *mcpServer) toolBuild(ctx context.Context, raw json.RawMessage, _ func(string, float64)) (string, error) {
	var a buildArgs
	if err := decodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	if (a.Design == "") == (a.DesignFile == "") {
		return "", errors.New("provide the design exactly one way — inline in design, or a saved file in design_file")
	}
	dir, err := resolveToolPath(s.root, a.Path)
	if err != nil {
		return "", err
	}
	design := a.Design
	if a.DesignFile != "" {
		p, err := resolveToolPath(s.root, a.DesignFile)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("reading design_file: %v", err)
		}
		design = string(data)
	}
	g, err := loadGrounding(dir, nil)
	if err != nil {
		return "", err
	}
	if _, err := s.mk(g.cfg.Provider, g.cfg.Model, 0.4, 16000); err != nil {
		return "", err
	}
	mk := func(mt int) Provider { p, _ := s.mk(g.cfg.Provider, g.cfg.Model, 0.4, mt); return p }
	sess := &session{ctx: ctx, mk: mk, root: dir, cfg: g.cfg, profile: g.profile, fileMap: g.fileMap, maxTokens: 8000}
	sess.orch = newOrchestrator(dir)

	res := sess.orch.runStream(ctx, sess.agentFor(RoleBuilder, ""),
		designTask(design, buildUserMessage(design, sess.profile, sess.fileMap)), nil, nil)
	if res.Err != nil {
		return "", res.Err
	}
	changes := parseFileBlocks(res.Output)
	if len(changes) == 0 {
		return "", errors.New("the model returned no file changes — the design may need a concrete file-by-file plan")
	}
	valid, previews, skipped := previewChanges(dir, changes)
	if len(valid) == 0 {
		return "", fmt.Errorf("every proposed path was unsafe and refused: %s", strings.Join(skipped, ", "))
	}

	if a.DryRun == nil || *a.DryRun {
		return renderBuildPreview(previews, skipped), nil
	}
	if gitDirty(dir) && !a.AllowDirty {
		return "", errors.New("repo has uncommitted changes — commit or stash them, or pass allow_dirty:true")
	}
	written, backupDir, err := writeChanges(dir, valid, time.Now())
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Wrote %d files", written)
	if backupDir != "" {
		fmt.Fprintf(&b, " · originals backed up to %s", backupDir)
	}
	b.WriteString("\n\n")
	b.WriteString(renderChangeTable(previews, skipped))
	return b.String(), nil
}

// renderBuildPreview renders the dry-run report: the change table, then a
// fenced diff per overwrite, capped at maxToolDiffLines lines each.
func renderBuildPreview(previews []changePreview, skipped []string) string {
	var b strings.Builder
	b.WriteString("## Proposed changes (dry run)\n\n")
	b.WriteString(renderChangeTable(previews, skipped))
	for _, p := range previews {
		if p.Diff == "" {
			continue
		}
		fmt.Fprintf(&b, "\n### %s (%s)\n\n```diff\n", p.Path, p.Kind)
		lines := strings.Split(strings.TrimSuffix(p.Diff, "\n"), "\n")
		shown := lines
		if len(shown) > maxToolDiffLines {
			shown = shown[:maxToolDiffLines]
		}
		b.WriteString(strings.Join(shown, "\n"))
		if n := len(lines) - len(shown); n > 0 {
			fmt.Fprintf(&b, "\n… %d more lines", n)
		}
		b.WriteString("\n```\n")
	}
	b.WriteString("\nNothing was written. Call again with dry_run:false to apply.\n")
	return b.String()
}

// renderChangeTable renders the kind/path/lines table plus any refused paths.
func renderChangeTable(previews []changePreview, skipped []string) string {
	var b strings.Builder
	b.WriteString("| change | path | lines |\n|---|---|---|\n")
	for _, p := range previews {
		lines := "—"
		if p.Kind != "delete" {
			lines = fmt.Sprintf("%d", p.Lines)
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", p.Kind, p.Path, lines)
	}
	for _, sk := range skipped {
		fmt.Fprintf(&b, "\n_refused unsafe path: `%s`_\n", sk)
	}
	return b.String()
}

// toolReview implements archi_review via the Phase 4 hook. A blocking verdict
// is content, not an error — the review itself succeeded.
func (s *mcpServer) toolReview(ctx context.Context, raw json.RawMessage, _ func(string, float64)) (string, error) {
	var a reviewArgs
	if err := decodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	dir, err := resolveToolPath(s.root, a.Path)
	if err != nil {
		return "", err
	}
	doc, _, err := serveReviewCore(ctx, dir, a.Patch)
	if err != nil {
		return "", err
	}
	return doc, nil
}

// toolRollback implements archi_rollback via the Phase 3 hook.
func (s *mcpServer) toolRollback(ctx context.Context, raw json.RawMessage, _ func(string, float64)) (string, error) {
	var a rollbackArgs
	if err := decodeToolArgs(raw, &a); err != nil {
		return "", err
	}
	if err := a.shape(); err != nil {
		return "", &errInvalidArgs{detail: err.Error()}
	}
	dir, err := resolveToolPath(s.root, a.Path)
	if err != nil {
		return "", err
	}
	return serveRollbackCore(dir, a.Run, a.List)
}
