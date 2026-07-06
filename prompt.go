package main

import (
	"fmt"
	"strings"
)

// seedFile is a repo file included in an LLM request as grounding context.
type seedFile struct {
	rel     string
	lang    string
	content string
}

const analystPrompt = `You are a staff engineer doing a fast, accurate read of an unfamiliar codebase.
You are given a repository map and the contents of its most important files. Produce
a concise, factual PROFILE another engineer (or an AI architect) can rely on to make
sound design decisions about THIS codebase. Do not invent things not evidenced by the
files. Where you infer, say "likely".

Output GitHub-Flavored Markdown with EXACTLY these sections, in order, no preamble:

## Stack
Languages, frameworks, runtimes, and datastores actually present, with the evidence
(which manifest or file shows each). One compact list or table.

## Architecture
2-4 sentences: the shape of the system (monolith, services, CLI, library, worker,
etc.) and how requests/data flow at a high level.

## Key modules
A table: Path | Responsibility. One row per significant directory or package, taken
from the real tree. Keep responsibilities to one line.

## Data & external systems
Datastores, queues, third-party APIs, and how the code talks to them (client libs,
config). If none are evident, say so.

## Conventions
The patterns a change should follow to fit in: error handling, config, testing,
naming, dependency style, how the app is built/run.

## Entry points & how to run
Where execution starts and the commands to build/test/run, drawn from manifests and
scripts.`

const refreshPrompt = `You are a staff engineer keeping an existing codebase profile up to date. You are
given the CURRENT PROFILE of a repository, its refreshed file map, a summary of what
changed since the profile was written, and the contents of the added and modified
files. Produce the UPDATED profile: keep everything still accurate, revise only what
the changes contradict, and fold in anything genuinely new. Do not invent things not
evidenced by the files. Where you infer, say "likely".

Output GitHub-Flavored Markdown with EXACTLY the same sections as the current
profile, in order, no preamble: ## Stack, ## Architecture, ## Key modules,
## Data & external systems, ## Conventions, ## Entry points & how to run.`

const architectPrompt = `You are a principal software architect embedded in a specific codebase. You are given
a PROFILE of that codebase, its file map, optionally some focus files, and a change
request. Produce ONE design document to implement that change so it fits THIS codebase
— its stack, structure, and conventions — that the team could start building tomorrow.

Rules:
- Ground everything in the given codebase. Reference REAL directories, packages, and
  files from the map/profile. Never propose a stack the repo doesn't use unless the
  request explicitly calls for introducing one, and if so, justify it.
- Be decisive: one design, not a menu. Alternatives are short trade-off notes.
- No speculative complexity. Prefer changes that match existing patterns.
- Diagrams are Mermaid, fenced with three backticks and the word mermaid, so they
  render on GitHub. Short node labels; avoid parentheses, quotes, and slashes inside
  labels.
- Output GitHub-Flavored Markdown with EXACTLY these sections, in order, no preamble.
  Begin with "## Summary".

## Summary
2-3 sentences: what is being built and the single key decision that makes it fit.

## How it fits
How this change maps onto the current architecture (name the real modules it touches).

## Changes by area
A table: Area/Path | Change | Notes. Use REAL paths from the codebase. Cover new files,
modified files, and deletions.

## New components
Any new services, packages, jobs, or modules — each with its responsibility and where
it lives in the tree. Write "None." if there are none.

## Diagrams
A Mermaid diagram of the proposed design in context (show existing pieces it connects
to). Add a second Mermaid sequence or ER diagram if the change has a critical flow or
data change.

## Data & schema changes
Migrations, new tables/fields, indexes, or message schemas. Write "None." if not
applicable.

## Build plan
A numbered, phased plan (smallest shippable slice first), 4-7 steps, each a concrete
deliverable referencing the paths above.

## Testing
What to test and how, matching the repo's existing test approach.

## Risks & rollout
The 3-5 biggest risks each with a one-line mitigation, plus how to roll it out safely
(flags, migration order, backfill, monitoring).`

// fenceFiles renders seed files as labeled, fenced blocks for a prompt.
func fenceFiles(files []seedFile) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "\n--- %s ---\n```%s\n%s\n```\n", f.rel, fenceLang(f.lang), strings.TrimRight(f.content, "\n"))
	}
	return b.String()
}

// analystUserMessage assembles the init request: the map plus seed files.
func analystUserMessage(mapMD string, seed []seedFile) string {
	var b strings.Builder
	b.WriteString("=== FILE MAP ===\n\n")
	b.WriteString(mapMD)
	b.WriteString("\n=== KEY FILES ===\n")
	b.WriteString(fenceFiles(seed))
	b.WriteString("\nWrite the codebase profile now.")
	return b.String()
}

// refreshUserMessage assembles the incremental-refresh request: the current
// profile, the regenerated map, the drift summary, and only what changed.
func refreshUserMessage(profileMD, mapMD, drift string, changed []seedFile, removed []string) string {
	var b strings.Builder
	b.WriteString("=== CURRENT PROFILE ===\n\n")
	b.WriteString(profileMD)
	b.WriteString("\n\n=== FILE MAP (refreshed) ===\n\n")
	b.WriteString(mapMD)
	b.WriteString("\n=== WHAT CHANGED SINCE THE PROFILE WAS WRITTEN ===\n\n")
	b.WriteString(drift + "\n")
	if len(removed) > 0 {
		b.WriteString("\nRemoved files:\n")
		for _, rel := range removed {
			b.WriteString("- " + rel + "\n")
		}
	}
	b.WriteString("\n=== ADDED & MODIFIED FILES ===\n")
	b.WriteString(fenceFiles(changed))
	b.WriteString("\nWrite the updated codebase profile now.")
	return b.String()
}

// designUserMessage assembles the design request: profile, map, an optional
// explorer brief, optional team memory, focus files, and the task. Empty brief
// and memory are omitted entirely, so fast-mode output stays byte-identical to
// before those were added.
func designUserMessage(profileMD, mapMD, brief, memory string, focus []seedFile, request string) string {
	var b strings.Builder
	b.WriteString("=== CODEBASE PROFILE ===\n\n")
	b.WriteString(profileMD)
	b.WriteString("\n\n=== FILE MAP ===\n\n")
	b.WriteString(mapMD)
	if brief != "" {
		b.WriteString("\n=== GROUNDING BRIEF (from the explorer) ===\n\n")
		b.WriteString(brief)
		b.WriteString("\n")
	}
	if memory != "" {
		b.WriteString("\n=== TEAM MEMORY (past decisions — honor them; don't re-ask or re-propose) ===\n\n")
		b.WriteString(memory)
		b.WriteString("\n")
	}
	if len(focus) > 0 {
		b.WriteString("\n=== FOCUS FILES ===\n")
		b.WriteString(fenceFiles(focus))
	}
	b.WriteString("\n=== CHANGE REQUEST ===\n\n")
	b.WriteString(request)
	b.WriteString("\n\nProduce the design document now, grounded in the codebase above.")
	return b.String()
}

const clarifyPrompt = `You are a principal architect embedded in a specific codebase. Before you design
anything, you interview the requester to remove the ambiguity that would otherwise
lead to the wrong design. You are given the codebase profile, its file map, and a
change request.

Ask the 3 to 6 MOST decision-critical questions — the ones whose answers would change
the architecture, not cosmetic preferences. Ground them in this codebase. For each,
give a one-line scenario showing why the answer matters (how the design forks on it),
and suggest concrete options.

Output ONLY question blocks in EXACTLY this format, separated by a line with three
dashes. No preamble, no numbering, nothing else:

Q: <the question>
Why: <one line — how the design changes depending on the answer>
Options: <comma-separated concrete options, or the word open>
---
Q: <next question>
Why: ...
Options: ...`

const answerPrompt = `You are a principal architect discussing a design you just produced for a specific
codebase. Answer the user's question directly and concisely, grounded in the design
and the codebase. Do NOT rewrite the design document — just answer. Use short
paragraphs or bullets. If the question reveals the design should change, say so in one
line at the end, prefixed "Suggest:".`

const buildPrompt = `You are a senior engineer implementing an APPROVED design in a specific codebase. You
are given the design, the codebase profile, and its file map. Produce the actual code
changes that implement the design, matching the codebase's existing conventions,
style, and structure.

Output ONLY file blocks — no prose, no explanation, before, between, or after. For
each file to create or replace, output its FULL contents:

=== FILE: relative/path/from/repo/root ===
` + "```" + `language
<complete file contents>
` + "```" + `

To delete a file, output a single line:

=== DELETE: relative/path ===

Rules: full file contents (never diffs or ellipses). Real paths from the map. Keep
changes minimal and idiomatic. Include any new tests the design calls for.`

// clarifyUserMessage assembles the interview request, with optional team memory.
func clarifyUserMessage(profileMD, mapMD string, focus []seedFile, request, memory string) string {
	return designUserMessage(profileMD, mapMD, "", memory, focus, request) + "\n\nAsk your clarifying questions now, in the required block format."
}

// reviseUserMessage asks the model to revise a design given requested changes.
func reviseUserMessage(design, changes string) string {
	return "=== CURRENT DESIGN ===\n\n" + design +
		"\n\n=== REQUESTED CHANGES ===\n\n" + changes +
		"\n\nOutput the FULL revised design document, same section structure, incorporating the changes."
}

// revisionUserMessage asks the designer to revise a design against critic
// findings — resolving blockers or arguing back in a Trade-off note.
func revisionUserMessage(design, verdicts string) string {
	return "=== CURRENT DESIGN ===\n\n" + design +
		"\n\n=== REVIEW FINDINGS ===\n\n" + verdicts +
		"\n\nRevise the design to resolve every blocking finding. For any blocking finding you" +
		"\ndisagree with, keep the design as is and argue against it in a short \"Trade-off:\"" +
		"\nnote inside the relevant section. Fold in advisory findings where they improve the" +
		"\ndesign; otherwise ignore them. Output the FULL revised design document, same section" +
		"\nstructure. Do not mention the reviewers or this revision process."
}

// questionUserMessage assembles a question about the current design.
func questionUserMessage(design, profileMD, question string) string {
	return "=== DESIGN ===\n\n" + design +
		"\n\n=== CODEBASE PROFILE ===\n\n" + profileMD +
		"\n\n=== QUESTION ===\n\n" + question + "\n\nAnswer now."
}

// buildUserMessage assembles the code-generation request from an approved design.
func buildUserMessage(design, profileMD, mapMD string) string {
	return "=== APPROVED DESIGN ===\n\n" + design +
		"\n\n=== CODEBASE PROFILE ===\n\n" + profileMD +
		"\n\n=== FILE MAP ===\n\n" + mapMD +
		"\n\nGenerate the code now, in the required file-block format only."
}

const plannerPrompt = `You are a technical lead splitting an APPROVED design into work packets for
parallel implementation. You are given the design, the codebase profile, and the
file map. Produce 2 to 8 packets. Each packet is a coherent unit one engineer could
implement alone: interfaces/types first, then implementations, then wiring and tests.
Packets must not share files. Dependencies must form a DAG — no cycles. Prefer fewer,
larger packets over many tiny ones.

Output ONLY one fenced block in EXACTLY this format, no prose before or after:

` + "```packets\n" + `- id: <short-slug>
  title: <one line>
  files: <comma-separated relative paths this packet creates or modifies>
  dependsOn: <comma-separated packet ids, or leave empty>
  notes: <one or two lines of guidance for the builder>
- id: <next packet>
` + "```" + `

Rules: ids are lowercase slugs, unique within the plan. Every dependsOn id must name
another packet in this plan. Use real paths from the map; new files go where the
design says. If the change is too small to split, output a single packet.`

const packetBuildPrompt = `You are a senior engineer implementing ONE work packet of an approved design in a
specific codebase. You are given the full design, your packet, the current contents
of the files your packet touches, and interface stubs from packets already completed.
Implement ONLY your packet. Do not touch files outside your packet's file list. Match
the codebase's existing conventions, style, and structure. Code against the provided
stubs exactly — never re-declare a symbol a stub defines.

Output ONLY file blocks — no prose before, between, or after. For each file to create
or replace, output its FULL contents:

=== FILE: relative/path/from/repo/root ===
` + "```language\n" + `<complete file contents>
` + "```" + `

To delete a file, output a single line:

=== DELETE: relative/path ===

Rules: full file contents (never diffs or ellipses). Only paths from your packet's
file list.`

const arbitratePrompt = `You are an integrator reconciling two generated versions of the SAME file, produced
by two work packets of one approved design. You are given the file's current content
on disk and both generated versions labeled with the packet that produced each.
Produce ONE merged file that preserves the intent of both packets and compiles. Where
the versions genuinely conflict, keep the behavior the design calls for and note the
discarded intent in a one-line code comment.

Output ONLY one file block, no prose:

=== FILE: <the path> ===
` + "```language\n" + `<complete merged file contents>
` + "```"

const repairPrompt = `You are a senior engineer fixing a build or test failure just introduced by generated
code. You are given the design summary, the failing command and its output, and the
current contents of the implicated files. Make the SMALLEST change that fixes the
failure without abandoning the design's intent. Do not refactor, rename, or improve
anything beyond the fix. If the true fix belongs in a file you were not given, output
that file's corrected FULL contents anyway, using its real path.

Output ONLY file blocks in the required format — full file contents, no prose:

=== FILE: relative/path ===
` + "```language\n" + `<complete file contents>
` + "```"

// plannerUserMessage assembles the planning request.
func plannerUserMessage(design, profileMD, mapMD string) string {
	return "=== APPROVED DESIGN ===\n\n" + design +
		"\n\n=== CODEBASE PROFILE ===\n\n" + profileMD +
		"\n\n=== FILE MAP ===\n\n" + mapMD +
		"\n\nProduce the work packets now, in the required fenced format only."
}

// packetUserMessage assembles one builder's request: design, its packet, the
// current contents of touched files, and stubs from completed packets.
func packetUserMessage(design string, pkt packet, files []seedFile, stubs string) string {
	var b strings.Builder
	b.WriteString("=== APPROVED DESIGN ===\n\n" + design + "\n\n")
	b.WriteString("=== YOUR PACKET ===\n")
	b.WriteString("id: " + pkt.ID + "\n")
	b.WriteString("title: " + pkt.Title + "\n")
	b.WriteString("files: " + strings.Join(pkt.Files, ", ") + "\n")
	if pkt.Notes != "" {
		b.WriteString("notes: " + pkt.Notes + "\n")
	}
	b.WriteString("\n=== CURRENT FILE CONTENTS ===\n")
	have := map[string]bool{}
	for _, f := range files {
		have[f.rel] = true
	}
	b.WriteString(fenceFiles(files))
	for _, f := range pkt.Files {
		if rel, ok := safeRel(f); ok && !have[rel] {
			b.WriteString("\n--- " + rel + " (new file) ---\n")
		}
	}
	if stubs != "" {
		b.WriteString("\n=== STUBS FROM COMPLETED PACKETS ===\n" + stubs)
	}
	b.WriteString("\n\nImplement your packet now, in the required file-block format only.")
	return b.String()
}

// arbitrateUserMessage assembles the two-version merge request.
func arbitrateUserMessage(path, base, a, b, pktA, pktB string) string {
	return "=== FILE ===\n" + path +
		"\n\n=== CURRENT ON DISK ===\n```\n" + base + "\n```\n" +
		"\n=== VERSION FROM " + pktA + " ===\n```\n" + a + "\n```\n" +
		"\n=== VERSION FROM " + pktB + " ===\n```\n" + b + "\n```\n" +
		"\n\nOutput the single merged file now."
}

// repairUserMessage assembles the repair request from a failing verification.
func repairUserMessage(designSummary, cmd, output string, files []seedFile) string {
	return "=== DESIGN (summary) ===\n\n" + designSummary +
		"\n\n=== FAILING COMMAND ===\n" + cmd +
		"\n\n=== OUTPUT ===\n" + output +
		"\n\n=== IMPLICATED FILES ===\n" + fenceFiles(files) +
		"\n\nOutput the minimal fix now, file blocks only."
}

// verdictTrailer is appended to every review prompt so parseVerdict applies.
const verdictTrailer = "\n\nEnd your review with EXACTLY one fenced block in this format (status is the worst\n" +
	"severity present; use \"clean\" with no findings when nothing is wrong):\n\n" +
	"```verdict\n" +
	"status: blocking\n" +
	"- [blocking] Title of finding | path/a.go, path/b.go\n" +
	"  One or two sentences of detail, ending with a concrete suggested fix.\n" +
	"- [advisory] Another finding | path/c.go\n" +
	"  Detail and suggested fix.\n" +
	"```"

const reviewSecPrompt = `You are a security reviewer examining a code diff. You may also be given a profile
of the codebase for context. Review ONLY what the diff changes — do not audit
unchanged code. Look for: injection (SQL, shell, template, path), missing or weakened
authn/authz, secrets or credentials in code or logs, unsafe deserialization, SSRF,
path traversal, insecure defaults, missing input validation at trust boundaries, and
crypto misuse. Every finding must name the file path(s) from the diff and include a
concrete suggested fix. Severity: "blocking" only for exploitable or data-exposing
issues; hardening ideas are "advisory". Do not pad — an empty review is a valid
review.` + verdictTrailer

const reviewSimpPrompt = `You are a simplicity reviewer examining a code diff. You may also be given a profile
of the codebase for context. Review ONLY what the diff changes. Look for:
over-engineering, premature abstraction, unnecessary new dependencies or infra,
duplicated logic that existing code already provides, dead or unreachable additions,
API surface larger than the need, and YAGNI violations. Every finding must name the
file path(s) from the diff and include a concrete simpler alternative. Severity:
"blocking" only when the complexity will actively mislead or break maintainers;
otherwise "advisory". Do not pad — an empty review is a valid review.` + verdictTrailer

const reviewGroundingPrompt = `You are a grounding reviewer examining a code diff against a PROFILE of the real
codebase. Your single job: find where the diff contradicts the actual repository —
calls to functions or files that do not exist, imports the project does not use,
violations of the stated conventions (error handling, config, naming, testing style),
and stack choices the profile does not support. Every finding MUST cite the file
path(s) involved; a finding without a path will be discarded. Severity: "blocking"
for references to things that do not exist; convention drift is "advisory". Do not
pad — an empty review is a valid review.` + verdictTrailer

const reviewerPrompt = `You are a senior engineer giving a single consolidated review of a code diff,
optionally with a profile of the codebase for context. Cover correctness, security,
and simplicity in one pass, reviewing ONLY what the diff changes. Every finding names
its file path(s) and a concrete suggested fix. Do not pad — an empty review is a
valid review.` + verdictTrailer

const testWriterPrompt = `You are a senior engineer writing tests for a change that was designed and built in
a specific codebase. You are given the approved design document, the codebase profile,
the file map, and the current contents of the files the change touches. Match the
repo's existing test framework, naming, and layout exactly (see the profile's
Conventions section) — never introduce a new test framework.

Output two parts, in order:

1. A test plan in GitHub-Flavored Markdown: "## Test plan" then a table
   Behavior | Kind (unit/integration) | File — covering the design's risk areas first.
2. The test files themselves, using EXACTLY this block format, full file contents,
   no prose between blocks:

=== FILE: relative/path/from/repo/root ===
` + "```language\n" + `<complete file contents>
` + "```" + `

Only create or replace test files. Never modify production source files.`

const docWriterPrompt = `You are a technical writer keeping a repository's documentation in sync with what
was recently designed and built. You are given the codebase profile, the current
README (possibly empty), existing docs pages, and the design documents produced since
the docs were last updated. Update the docs to reflect what those designs added or
changed: new commands, flags, endpoints, modules, configuration, and behavior. Keep
the existing voice, structure, and formatting; make the smallest edits that restore
accuracy; never delete sections you cannot verify are obsolete; never invent features
the designs do not describe.

Output ONLY file blocks (full contents, no prose outside blocks):

=== FILE: relative/path ===
` + "```markdown\n" + `<complete file contents>
` + "```" + `

Only touch documentation files (README.md, docs/**). If nothing needs updating,
output nothing at all.`

const memorySummarizerPrompt = `You distill one completed design-and-build session into a single durable memory
entry for future sessions. You are given the final design document, the interview
transcript (requester Q&A, possibly empty), and the build outcome. Extract only what
will still matter months from now — decisions, rejections, constraints, and stable
facts about the team or system ("deploys on Fly.io", "no new infra without approval").
Never include transient details (line counts, token counts, model names) or anything
speculative.

Output EXACTLY these four fields, no heading, no preamble, nothing else:

**Decided:** <one or two sentences — the approach chosen and the key reason>
**Rejected:** <alternatives considered and why they lost; "None recorded." if none>
**Constraints:** <hard constraints revealed, comma-separated; "None recorded." if none>
**Facts:**
- <one durable fact per line, as a "- " bullet; omit the bullets entirely if none>`

const memoryCompactorPrompt = `You are compacting a team memory file of dated "## <date> <slug>" entries, each with
**Decided / Rejected / Constraints / Facts** fields. Rewrite the file so it is smaller
but loses no decision: merge entries about the same topic into the newest entry
(keeping its heading), delete duplicate or superseded facts, and drop entries fully
obsoleted by a later decision. Preserve chronological order, the exact heading format,
and the exact four-field format. Never invent, reword facts beyond merging, or add
commentary. Output ONLY the rewritten file contents.`

// reviewSpecs returns the three review-critic specs (reusing the critic roles).
func reviewSpecs() []AgentSpec {
	return []AgentSpec{
		{Role: RoleSecCritic, System: reviewSecPrompt, MaxTokens: 2000, Temp: 0},
		{Role: RoleSimpCritic, System: reviewSimpPrompt, MaxTokens: 2000, Temp: 0},
		{Role: RoleGrndCritic, System: reviewGroundingPrompt, MaxTokens: 2000, Temp: 0},
	}
}

// reviewUserMessage assembles a diff-review request.
func reviewUserMessage(profile, diff string) string {
	var b strings.Builder
	if profile != "" {
		b.WriteString("=== CODEBASE PROFILE ===\n\n" + profile + "\n\n")
	}
	b.WriteString("=== DIFF ===\n\n```diff\n" + diff + "\n```\n\nReview the diff now.")
	return b.String()
}

// testPlanUserMessage assembles a test-generation request for a saved design.
func testPlanUserMessage(profile, design, mapMD string, touched []seedFile, memory string) string {
	var b strings.Builder
	b.WriteString("=== CODEBASE PROFILE ===\n\n" + profile + "\n\n")
	b.WriteString("=== APPROVED DESIGN ===\n\n" + design + "\n\n")
	b.WriteString("=== FILE MAP ===\n\n" + mapMD + "\n")
	if memory != "" {
		b.WriteString("\n=== TEAM MEMORY ===\n\n" + memory + "\n")
	}
	b.WriteString("\n=== FILES THE CHANGE TOUCHES ===\n" + fenceFiles(touched))
	b.WriteString("\n\nWrite the test plan and test files now.")
	return b.String()
}

// docUserMessage assembles a documentation-sync request.
func docUserMessage(profile, readme string, docs, designs []seedFile) string {
	var b strings.Builder
	b.WriteString("=== CODEBASE PROFILE ===\n\n" + profile + "\n\n")
	b.WriteString("=== CURRENT README ===\n```markdown\n" + readme + "\n```\n")
	if len(docs) > 0 {
		b.WriteString("\n=== EXISTING DOCS ===\n" + fenceFiles(docs))
	}
	b.WriteString("\n=== DESIGNS SINCE LAST DOC UPDATE ===\n" + fenceFiles(designs))
	b.WriteString("\n\nUpdate the documentation now.")
	return b.String()
}

// memorySummaryUserMessage assembles the summarizer request.
func memorySummaryUserMessage(design, interview, outcome string) string {
	var b strings.Builder
	b.WriteString("=== FINAL DESIGN ===\n\n" + design + "\n\n")
	if interview != "" {
		b.WriteString("=== INTERVIEW ===\n\n" + interview + "\n\n")
	}
	b.WriteString("=== BUILD OUTCOME ===\n\n" + outcome + "\n\nDistill the memory entry now.")
	return b.String()
}
