package main

import (
	"context"
	"errors"
	"strings"
)

// Explorer sizing. A repo whose sampled token weight is below
// explorerMinRepoTokens fits the classic profile+map+focus grounding, so the
// explorer adds latency without adding signal — skip it.
const (
	explorerMinRepoTokens = 32000      // skip explorer when cfg.ApproxTokens < this
	maxExplorerFiles      = 12         // hard cap on step-1 file requests
	maxExplorerFileBytes  = 128 * 1024 // per-file read cap
	explorerFilesTokens   = 24000      // fitBudget cap for requested-file contents
)

const explorerPickPrompt = `You are a staff engineer orienting in an unfamiliar codebase before a design
session. You are given a codebase profile, its file map, and a change request
(with any clarifications from the requester). Pick the files whose CONTENTS you
need to read to brief the architect accurately: the files the change will
touch, the patterns it must follow, and anything it could break.

Rules:
- Choose only paths that appear in the file map, exactly as written there.
- At most 12 files. Prefer small, central files over large or generated ones.
- No lock files, vendored code, or generated artifacts.

Output ONLY a fenced files block, one path per line, nothing else:

` + "```files\n" + `real/path/from/map.go
another/real/path.py
` + "```"

const explorerBriefPrompt = `You are a staff engineer briefing a software architect before they design a
change to a codebase you have just studied. You are given the codebase profile,
its file map, the change request (with any clarifications), and the contents of
the files you asked to read. Produce a GROUNDING BRIEF: the request-specific
context the architect needs to design a change that fits THIS codebase. Be
factual and cite real paths; do not invent files or APIs. Where you infer, say
"likely".

Output GitHub-Flavored Markdown with EXACTLY these sections, in order, no
preamble:

## Relevant modules
A table: Path | Role in this change. Only what matters for THIS request.

## Patterns to follow
The existing conventions the change must copy — error handling, naming,
layering, testing — each with the file that demonstrates it.

## Landmines
Traps in the current code a naive design would hit: hidden couplings,
invariants, concurrency, ordering, compatibility. Each with the path that
proves it.

## Suggested seams
Where the change most naturally attaches — the functions, interfaces, or files
to extend rather than replace, with paths.`

// explorerWanted reports whether the ensemble should run the explorer.
func explorerWanted(cfg *Config) bool { return cfg.ApproxTokens >= explorerMinRepoTokens }

// parseFileRequests extracts the LAST ```files fenced block: one path per line,
// trimmed; blank lines and comments dropped; duplicates removed preserving
// first occurrence; clamped to maxExplorerFiles. Missing/empty block => nil.
func parseFileRequests(s string) []string {
	block, ok := lastFencedBlock(s, "files")
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(block, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || strings.HasPrefix(p, "#") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxExplorerFiles {
			break
		}
	}
	return out
}

// readRequestedFiles resolves step-1 requests against the real repo: only paths
// present in scanRepo are read (this is the path-traversal guard); each is read
// (capped, binary-safe) and redacted. dropped counts requests that didn't
// resolve — absent from the scan, unreadable, or over the size cap.
func readRequestedFiles(root string, reqs []string) (files []seedFile, dropped int) {
	scanned, err := scanRepo(root)
	if err != nil {
		return nil, len(reqs)
	}
	inScan := make(map[string]fileInfo, len(scanned))
	for _, f := range scanned {
		inScan[f.rel] = f
	}
	for _, rel := range reqs {
		fi, ok := inScan[rel]
		if !ok {
			dropped++
			continue
		}
		data, okRead := readTextFile(root, rel, maxExplorerFileBytes)
		if !okRead {
			dropped++
			continue
		}
		files = append(files, seedFile{rel: rel, lang: fi.lang, content: redactForPrompt(rel, string(data))})
	}
	return files, dropped
}

// runExplorer executes the two-step explorer on the design model and returns a
// grounding brief plus the number of files it read. Any failure — LLM error, no
// file requests, or zero resolved files — returns ("", 0, err) so the caller
// falls back to classic grounding.
func runExplorer(ctx context.Context, o *Orchestrator, mk providerFactory, root, profile, fileMap, request, interview string) (string, int, error) {
	base := []budgetPart{
		{name: "codebase profile", required: true, content: profile},
		{name: "file map", content: fileMap},
	}

	// Step 1 — pick files.
	pick := Agent{Spec: AgentSpec{Role: RoleExplorer, System: explorerPickPrompt, MaxTokens: 800, Temp: 0}, P: mk(800)}
	r1 := o.Run(ctx, pick, TaskInput{Request: request, Interview: interview, Grounding: base})
	if r1.Err != nil {
		return "", 0, r1.Err
	}
	reqs := parseFileRequests(r1.Output)
	if len(reqs) == 0 {
		return "", 0, errors.New("explorer requested no files")
	}
	files, _ := readRequestedFiles(root, reqs)
	if len(files) == 0 {
		return "", 0, errors.New("explorer requested no readable files")
	}

	// Step 2 — brief. Fit the file contents to their own budget first, so they
	// can't starve the profile/map that frame them.
	fileParts := make([]budgetPart, 0, len(files))
	for _, f := range files {
		fileParts = append(fileParts, budgetPart{name: f.rel, content: fenceFiles([]seedFile{f})})
	}
	fitted, _ := fitBudget(fileParts, explorerFilesTokens)
	grounding := append([]budgetPart{}, base...)
	grounding = append(grounding, fitted...)

	brief := Agent{Spec: AgentSpec{Role: RoleExplorer, System: explorerBriefPrompt, MaxTokens: 2500, Temp: 0}, P: mk(2500)}
	r2 := o.Run(ctx, brief, TaskInput{Request: request, Interview: interview, Grounding: grounding})
	if r2.Err != nil {
		return "", 0, r2.Err
	}
	return r2.Output, len(files), nil
}
