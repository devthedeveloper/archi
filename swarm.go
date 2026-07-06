package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// printPlan renders the packet plan to stderr.
func printPlan(ps []packet) {
	for i, p := range ps {
		line := fmt.Sprintf("    %d. %s  %s", i+1, cyan(p.ID), dim(p.Title))
		if len(p.DependsOn) > 0 {
			line += dim("  ← " + strings.Join(p.DependsOn, ", "))
		}
		fmt.Fprintln(os.Stderr, line)
	}
}

// reportPackets summarizes the swarm outcome: how many built, which didn't.
func reportPackets(results []packetResult) {
	built := 0
	var notBuilt []string
	for _, r := range results {
		if r.err == nil {
			built++
			continue
		}
		reason := "LLM error"
		if r.skipped {
			reason = "skipped — " + strings.TrimPrefix(r.err.Error(), "skipped: ")
		}
		notBuilt = append(notBuilt, r.pkt.ID+" ("+reason+")")
	}
	ok(fmt.Sprintf("%d packets built", built))
	if len(notBuilt) > 0 {
		warn(fmt.Sprintf("%d packets not built: %s", len(notBuilt), strings.Join(notBuilt, ", ")))
	}
}

// packetResult is one packet's build outcome.
type packetResult struct {
	pkt     packet
	changes []fileChange // parsed and filtered to pkt.Files
	err     error        // LLM error, empty output, or "skipped: dep <id> failed"
	skipped bool
}

// builderRole is the per-packet Board display role, "builder:<id>".
func builderRole(id string) AgentRole { return AgentRole("builder:" + id) }

// runSwarm executes plan levels in dependency order with a fault-tolerant pool.
// Per-packet errors mark the packet failed and its transitive dependents
// skipped; only ctx cancellation aborts the whole swarm. Returns the merged
// changes ready for applyChanges plus per-packet results for reporting.
func runSwarm(ctx context.Context, o *Orchestrator, s *session, design string, ps []packet) ([]fileChange, []packetResult, error) {
	levels, err := topoLevels(ps)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]packet, len(ps))
	roles := make([]AgentRole, 0, len(ps))
	for _, p := range ps {
		byID[p.ID] = p
		roles = append(roles, builderRole(p.ID))
	}

	board := newBoard(roles)
	o.Board = board
	defer func() { board.Stop(); o.Board = nil }()

	status := make(map[string]string, len(ps)) // pending|ok|failed|skipped
	resByID := make(map[string]*packetResult, len(ps))
	for _, p := range ps {
		status[p.ID] = "pending"
	}
	var stubs strings.Builder

	for _, level := range levels {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		var calls []AgentCall
		var callIDs []string
		for _, id := range level {
			pkt := byID[id]
			depFailed := ""
			for _, d := range pkt.DependsOn {
				if status[d] != "ok" {
					depFailed = d
					break
				}
			}
			if depFailed != "" {
				status[id] = "skipped"
				resByID[id] = &packetResult{pkt: pkt, skipped: true, err: fmt.Errorf("skipped: dependency %s failed", depFailed)}
				o.Board.Set(builderRole(id), "failed", "skipped — dep "+depFailed)
				continue
			}
			agent := Agent{Spec: AgentSpec{Role: builderRole(id), System: packetBuildPrompt, MaxTokens: 8000, Temp: 0.2}, P: s.builder()}
			calls = append(calls, AgentCall{Agent: agent, Input: builderInput(s.root, design, pkt, stubs.String())})
			callIDs = append(callIDs, id)
		}
		if len(calls) == 0 {
			continue
		}
		results := o.RunPool(ctx, calls)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		for i, res := range results {
			id := callIDs[i]
			pkt := byID[id]
			if res.Err != nil {
				status[id] = "failed"
				resByID[id] = &packetResult{pkt: pkt, err: res.Err}
				continue
			}
			changes := filterToPacket(pkt, parseFileBlocks(res.Output))
			if len(changes) == 0 {
				status[id] = "failed"
				resByID[id] = &packetResult{pkt: pkt, err: fmt.Errorf("builder produced no usable files")}
				continue
			}
			status[id] = "ok"
			resByID[id] = &packetResult{pkt: pkt, changes: changes}
			stubs.WriteString(extractStubs(id, changes))
		}
	}

	results := make([]packetResult, 0, len(ps))
	okCount := 0
	for _, p := range ps {
		r := resByID[p.ID]
		if r == nil {
			r = &packetResult{pkt: p, err: fmt.Errorf("not built")}
		}
		if status[p.ID] == "ok" {
			okCount++
		}
		results = append(results, *r)
	}
	if okCount == 0 {
		return nil, results, fmt.Errorf("no packets built")
	}
	merged, err := mergeOutputs(s.root, results, ps, s.arbitrate)
	if err != nil {
		return nil, results, err
	}
	return merged, results, nil
}

// filterToPacket keeps only changes whose path is in the packet's file list
// (and safe), warning about any the builder emitted outside its scope.
func filterToPacket(pkt packet, changes []fileChange) []fileChange {
	allow := make(map[string]bool, len(pkt.Files))
	for _, f := range pkt.Files {
		if rel, ok := safeRel(f); ok {
			allow[rel] = true
		}
	}
	var out []fileChange
	for _, c := range changes {
		rel, ok := safeRel(c.path)
		if !ok || !allow[rel] {
			warn("packet " + pkt.ID + ": dropping out-of-scope file " + c.path)
			continue
		}
		c.path = rel
		out = append(out, c)
	}
	return out
}

// builderInput assembles one builder's TaskInput via the escape hatch.
func builderInput(root, design string, pkt packet, stubs string) TaskInput {
	seeds := fitSeeds(readSeeds(root, existingFiles(root, pkt.Files)), contextBudget())
	return TaskInput{Request: "implement packet " + pkt.ID, Extra: map[string]string{
		"user_message": packetUserMessage(design, pkt, seeds, stubs)}}
}

// existingFiles returns the subset of rels that exist on disk.
func existingFiles(root string, rels []string) []string {
	var out []string
	for _, rel := range rels {
		if _, ok := readTextFile(root, rel, 256*1024); ok {
			out = append(out, rel)
		}
	}
	return out
}

// extractStubs summarizes a builder's output for dependents: for Go files, the
// package line plus zero-indent func/type/const/var declarations; other files,
// their first 40 lines. Deleted files contribute nothing.
func extractStubs(pktID string, changes []fileChange) string {
	var b strings.Builder
	for _, c := range changes {
		if c.del {
			continue
		}
		fmt.Fprintf(&b, "// from %s (packet %s)\n", c.path, pktID)
		if strings.HasSuffix(c.path, ".go") {
			b.WriteString(goStubs(c.content))
		} else {
			b.WriteString(headLines(c.content, 40))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// goStubs extracts the package line and zero-indent declarations from Go source.
func goStubs(src string) string {
	lines := strings.Split(src, "\n")
	var out []string
	inTypeBlock := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "package "):
			out = append(out, line)
		case inTypeBlock:
			out = append(out, line)
			if strings.HasPrefix(line, "}") {
				inTypeBlock = false
			}
		case strings.HasPrefix(line, "func "), strings.HasPrefix(line, "const "), strings.HasPrefix(line, "var "):
			out = append(out, line)
		case strings.HasPrefix(line, "type "):
			out = append(out, line)
			if strings.HasSuffix(strings.TrimSpace(line), "{") {
				inTypeBlock = true
			}
		}
		if len(out) >= 100 {
			break
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// headLines returns the first n lines of s.
func headLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n") + "\n"
}

// readSeeds reads and redacts the given files into seedFiles, skipping any that
// can't be read.
func readSeeds(root string, rels []string) []seedFile {
	var out []seedFile
	for _, rel := range rels {
		data, ok := readTextFile(root, rel, 256*1024)
		if !ok {
			continue
		}
		out = append(out, seedFile{rel: rel, lang: langFor(rel), content: redactForPrompt(rel, string(data))})
	}
	return out
}

// fitSeeds fits seed file contents into a token budget, preserving order.
func fitSeeds(seeds []seedFile, budget int) []seedFile {
	parts := make([]budgetPart, len(seeds))
	for i, s := range seeds {
		parts[i] = budgetPart{name: s.rel, content: s.content}
	}
	fitted, _ := fitBudget(parts, budget)
	byRel := make(map[string]seedFile, len(seeds))
	for _, s := range seeds {
		byRel[s.rel] = s
	}
	var out []seedFile
	for _, p := range fitted {
		sf := byRel[p.name]
		sf.content = p.content
		out = append(out, sf)
	}
	return out
}

// arbitrate is the session's LLM arbitration call for merge conflicts.
func (s *session) arbitrate(path, base, a, b string) (string, error) {
	agent := Agent{Spec: AgentSpec{Role: RoleIntegrator, System: arbitratePrompt, MaxTokens: 8000, Temp: 0}, P: s.builder()}
	in := TaskInput{Request: "merge " + path, Extra: map[string]string{
		"user_message": arbitrateUserMessage(path, base, a, b, "version A", "version B")}}
	res := s.orch.Run(s.ctx, agent, in)
	if res.Err != nil {
		return "", res.Err
	}
	blocks := parseFileBlocks(res.Output)
	for _, c := range blocks {
		if rel, ok := safeRel(c.path); ok && rel == path {
			return c.content, nil
		}
	}
	if len(blocks) == 1 {
		return blocks[0].content, nil
	}
	return "", fmt.Errorf("arbitration returned no block for %s", path)
}

// mergeOutputs merges all ok packets' changes into one []fileChange, resolving
// same-path conflicts by dependency order, then textual union, then arbitration.
func mergeOutputs(root string, results []packetResult, ps []packet, arb func(path, base, a, b string) (string, error)) ([]fileChange, error) {
	closure := dependencyClosure(ps)
	byPath := map[string][]struct {
		pktIdx int
		change fileChange
	}{}
	order := []string{}
	pktIndex := map[string]int{}
	for i, p := range ps {
		pktIndex[p.ID] = i
	}
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for _, c := range r.changes {
			if _, seen := byPath[c.path]; !seen {
				order = append(order, c.path)
			}
			byPath[c.path] = append(byPath[c.path], struct {
				pktIdx int
				change fileChange
			}{pktIndex[r.pkt.ID], c})
		}
	}

	var merged []fileChange
	for _, path := range order {
		writers := byPath[path]
		if len(writers) == 1 {
			merged = append(merged, writers[0].change)
			continue
		}
		// Fold left, pairwise.
		acc := writers[0]
		for _, w := range writers[1:] {
			resolved, err := resolvePair(root, path, acc, w, ps, closure, arb)
			if err != nil {
				return nil, err
			}
			acc = resolved
		}
		merged = append(merged, acc.change)
	}
	return merged, nil
}

type pathWriter = struct {
	pktIdx int
	change fileChange
}

// resolvePair resolves two writers of the same path.
func resolvePair(root, path string, a, b pathWriter, ps []packet, closure map[string]map[string]bool, arb func(path, base, a, b string) (string, error)) (pathWriter, error) {
	aID, bID := ps[a.pktIdx].ID, ps[b.pktIdx].ID
	// 1. Dependency order wins.
	if closure[aID][bID] {
		return a, nil // a depends on b, a built later with b's stubs
	}
	if closure[bID][aID] {
		return b, nil
	}
	// A delete with no dependency relation: prefer the later-in-plan writer.
	if a.change.del || b.change.del {
		if b.pktIdx >= a.pktIdx {
			warn("merge: " + path + " has a delete and a write — kept " + bID)
			return b, nil
		}
		return a, nil
	}
	// 2. Textual union.
	base := ""
	if data, ok := readTextFile(root, path, 256*1024); ok {
		base = string(data)
	}
	if u, ok := mergeDisjoint(base, a.change.content, b.change.content); ok {
		c := a.change
		c.content = u
		return pathWriter{pktIdx: max2(a.pktIdx, b.pktIdx), change: c}, nil
	}
	// 3. Arbitration.
	if arb != nil {
		if merged, err := arb(path, base, a.change.content, b.change.content); err == nil {
			c := a.change
			c.content = merged
			return pathWriter{pktIdx: max2(a.pktIdx, b.pktIdx), change: c}, nil
		}
	}
	later, keptID := b, bID
	if a.pktIdx > b.pktIdx {
		later, keptID = a, aID
	}
	warn("arbitration failed for " + path + " — kept " + keptID + "'s version")
	return later, nil
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// dependencyClosure computes the transitive dependsOn closure per packet id.
func dependencyClosure(ps []packet) map[string]map[string]bool {
	direct := map[string][]string{}
	for _, p := range ps {
		direct[p.ID] = p.DependsOn
	}
	closure := map[string]map[string]bool{}
	var dfs func(id string, acc map[string]bool)
	dfs = func(id string, acc map[string]bool) {
		for _, d := range direct[id] {
			if !acc[d] {
				acc[d] = true
				dfs(d, acc)
			}
		}
	}
	for _, p := range ps {
		acc := map[string]bool{}
		dfs(p.ID, acc)
		closure[p.ID] = acc
	}
	return closure
}

// ── textual union via disjoint diff segments ────────────────────────────────

// seg is a base-line range [start,end) replaced by lines.
type seg struct {
	start, end int
	lines      []string
}

// mergeDisjoint attempts a textual union of two rewrites of base. If no change
// region of a overlaps a change region of b, both are spliced into base and
// (merged, true) is returned; otherwise ("", false). Oversized inputs decline.
func mergeDisjoint(base, a, b string) (string, bool) {
	baseLines := splitDiffLines(base)
	aLines := splitDiffLines(a)
	bLines := splitDiffLines(b)
	if len(baseLines) > maxDiffLines || len(aLines) > maxDiffLines || len(bLines) > maxDiffLines {
		return "", false
	}
	segsA := diffSegments(diffOps(baseLines, aLines))
	segsB := diffSegments(diffOps(baseLines, bLines))
	for _, sa := range segsA {
		for _, sb := range segsB {
			if segsOverlap(sa, sb) {
				return "", false
			}
		}
	}
	all := append(append([]seg{}, segsA...), segsB...)
	merged := spliceSegments(baseLines, all)
	if len(merged) == 0 {
		return "", true
	}
	return strings.Join(merged, "\n") + "\n", true
}

// diffSegments folds a base→x edit script into replacement segments over base.
func diffSegments(ops []diffOp) []seg {
	var segs []seg
	baseIdx := 0
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			baseIdx++
			i++
			continue
		}
		s := seg{start: baseIdx}
		for i < len(ops) && ops[i].kind != ' ' {
			if ops[i].kind == '-' {
				baseIdx++
			} else {
				s.lines = append(s.lines, ops[i].line)
			}
			i++
		}
		s.end = baseIdx
		segs = append(segs, s)
	}
	return segs
}

// segsOverlap conservatively reports whether two base segments conflict.
// Insertions (zero-width) at the same index conflict; anything touching or
// intersecting conflicts (bias toward "overlap" keeps merges safe).
func segsOverlap(a, b seg) bool {
	aIns, bIns := a.start == a.end, b.start == b.end
	switch {
	case aIns && bIns:
		return a.start == b.start
	case aIns:
		return a.start >= b.start && a.start <= b.end
	case bIns:
		return b.start >= a.start && b.start <= a.end
	default:
		return a.start <= b.end && b.start <= a.end
	}
}

// spliceSegments applies non-overlapping segments to base, left to right.
func spliceSegments(base []string, segs []seg) []string {
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].start != segs[j].start {
			return segs[i].start < segs[j].start
		}
		return segs[i].end < segs[j].end
	})
	var out []string
	pos := 0
	for _, s := range segs {
		if s.start > pos {
			out = append(out, base[pos:s.start]...)
		}
		out = append(out, s.lines...)
		if s.end > pos {
			pos = s.end
		}
	}
	if pos < len(base) {
		out = append(out, base[pos:]...)
	}
	return out
}
