package main

import "strings"

// criticRoles is the panel, in display and render order.
var criticRoles = []AgentRole{RoleSecCritic, RoleSimpCritic, RoleGrndCritic}

// verdictTail is appended to every critic prompt: the exact verdict wire format
// they must emit. Built by concatenation because it embeds a fenced block.
const verdictTail = "End your review with a fenced verdict block in EXACTLY this format. status is\n" +
	"blocking if ANY finding is blocking, advisory if only advisory findings remain,\n" +
	"and clean if there are none (a clean verdict has no finding lines):\n\n" +
	"```verdict\n" +
	"status: blocking\n" +
	"- [blocking] Short title of the finding | path/one.go, path/two.go\n" +
	"  One or two sentences of detail.\n" +
	"- [advisory] Another finding | path/three.go\n" +
	"  Detail.\n" +
	"```\n\n" +
	"Severities: blocking means the design should not be built as written; advisory\n" +
	"means worth noting, not worth stopping for. Never invent findings to look\n" +
	"thorough — a clean verdict is a good verdict."

const secCriticPrompt = `You are a security engineer reviewing a design document before it is built. You
are given the codebase profile, grounding context, the change request, and the
design. Judge ONLY security: injection (SQL, command, template, path), authn
and authz gaps, secret and credential handling, unsafe defaults, missing
validation at trust boundaries, sensitive data in logs or errors, and insecure
transport or storage.

Rules:
- Review the DESIGN as written, not hypothetical implementations of it.
- Flag only issues plausible in THIS codebase and this change — no generic
  checklists.
- Blocking is reserved for flaws that would ship a vulnerability if built as
  designed.
- Do not comment on style, complexity, or factual grounding — other reviewers
  own those.`

const simpCriticPrompt = `You are a pragmatic staff engineer reviewing a design document for
over-engineering before it is built. You are given the codebase profile,
grounding context, the change request, and the design. Judge ONLY simplicity:
components the request does not need, new infrastructure where existing pieces
would do, premature abstraction or generality, speculative configuration and
flags, and build plans longer than the change warrants.

Rules:
- The bar is the REQUEST: complexity the request itself demands is not a
  finding.
- Prefer concrete simplifications ("fold X into the existing Y in path/z.go")
  over taste.
- Blocking is reserved for complexity that would materially slow delivery or
  burden maintenance; preferences are advisory.
- Do not comment on security or factual grounding — other reviewers own those.`

const grndCriticPrompt = `You are a fact-checker for design documents. You are given the codebase
profile, its file map, grounding context, the change request, and the design.
Judge ONLY grounding: claims about the codebase that the map and grounding do
not support — files or directories that do not exist, functions or APIs the
design invents, misdescribed behavior of existing modules, and conventions the
design attributes to the repo that it does not follow.

Rules:
- Verify every path in the design's "Changes by area" table against the file
  map. Paths for NEW files are fine when their parent directory exists or the
  design creates it.
- EVERY finding must cite at least one real path from the file map as
  evidence, after the | in the finding line. A finding you cannot pin to a
  real path does not belong in your verdict.
- Blocking is reserved for errors that would send an implementer to files or
  APIs that do not exist; smaller inaccuracies are advisory.
- Do not comment on security or simplicity — other reviewers own those.`

// criticSpec returns the AgentSpec for a critic role: its system prompt plus
// the shared verdict tail, at temperature 0.
func criticSpec(role AgentRole) AgentSpec {
	var sys string
	switch role {
	case RoleSecCritic:
		sys = secCriticPrompt
	case RoleSimpCritic:
		sys = simpCriticPrompt
	case RoleGrndCritic:
		sys = grndCriticPrompt
	}
	return AgentSpec{Role: role, System: sys + "\n\n" + verdictTail, MaxTokens: 1500, Temp: 0}
}

// parseVerdict extracts the LAST ```verdict fenced block. A missing block
// yields an advisory verdict noting the absence, so agreement-theater is
// visible. Status is always recomputed from the findings, overriding any
// declared status. Never returns a nil verdict with a nil error.
func parseVerdict(s string) (*Verdict, error) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	block, ok := lastFencedBlock(s, "verdict")
	if !ok {
		return &Verdict{
			Status:   VerdictAdvisory,
			Findings: []Finding{{Severity: VerdictAdvisory, Title: "critic returned no verdict block"}},
		}, nil
	}
	v := &Verdict{}
	var cur *Finding
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "status:"):
			// Declared status is informational; recomputed below.
		case strings.HasPrefix(trimmed, "- ["):
			v.Findings = append(v.Findings, parseFindingLine(trimmed))
			cur = &v.Findings[len(v.Findings)-1]
		default:
			if cur != nil && trimmed != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
				if cur.Detail == "" {
					cur.Detail = trimmed
				} else {
					cur.Detail += " " + trimmed
				}
			}
		}
	}
	v.Status = recomputeStatus(v.Findings)
	return v, nil
}

// parseFindingLine parses "- [sev] Title | p1, p2". Any severity other than
// blocking is advisory; a missing "|" leaves Paths nil.
func parseFindingLine(line string) Finding {
	rest := strings.TrimPrefix(line, "- [")
	f := Finding{Severity: VerdictAdvisory}
	sevEnd := strings.IndexByte(rest, ']')
	if sevEnd < 0 {
		f.Title = strings.TrimSpace(rest)
		return f
	}
	if strings.EqualFold(strings.TrimSpace(rest[:sevEnd]), "blocking") {
		f.Severity = VerdictBlocking
	}
	body := strings.TrimSpace(rest[sevEnd+1:])
	if pipe := strings.IndexByte(body, '|'); pipe >= 0 {
		f.Title = strings.TrimSpace(body[:pipe])
		for _, p := range strings.Split(body[pipe+1:], ",") {
			if p = strings.TrimSpace(p); p != "" {
				f.Paths = append(f.Paths, p)
			}
		}
	} else {
		f.Title = body
	}
	return f
}

// recomputeStatus derives a verdict status from its findings.
func recomputeStatus(findings []Finding) VerdictStatus {
	blocking, any := false, false
	for _, f := range findings {
		any = true
		if f.Severity == VerdictBlocking {
			blocking = true
		}
	}
	switch {
	case blocking:
		return VerdictBlocking
	case any:
		return VerdictAdvisory
	default:
		return VerdictClean
	}
}

// downgradePathless enforces the grounding critic's citation rule: any blocking
// finding with no cited path is downgraded to advisory. Returns how many were
// downgraded and recomputes the status.
func downgradePathless(v *Verdict) int {
	n := 0
	for i := range v.Findings {
		if v.Findings[i].Severity == VerdictBlocking && len(v.Findings[i].Paths) == 0 {
			v.Findings[i].Severity = VerdictAdvisory
			n++
		}
	}
	if n > 0 {
		v.Status = recomputeStatus(v.Findings)
	}
	return n
}

// lastFencedBlock returns the body of the last ```<lang> fenced block in s.
func lastFencedBlock(s, lang string) (string, bool) {
	open := "```" + lang
	lines := strings.Split(s, "\n")
	start := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == open {
			start = i
		}
	}
	if start < 0 {
		return "", false
	}
	var body []string
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			return strings.Join(body, "\n"), true
		}
		body = append(body, lines[i])
	}
	return strings.Join(body, "\n"), true
}
