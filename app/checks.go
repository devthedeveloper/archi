package main

import (
	"fmt"
	"strings"
)

// checkRun is the POST /repos/{repo}/check-runs body.
type checkRun struct {
	Name       string      `json:"name"` // always "archi review"
	HeadSHA    string      `json:"head_sha"`
	Status     string      `json:"status"`               // "in_progress" | "completed"
	Conclusion string      `json:"conclusion,omitempty"` // success | neutral | failure
	Output     checkOutput `json:"output"`
}

type checkOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"` // ≤65535 chars — truncateCheckText
}

// checkName is the single check run name the app publishes.
const checkName = "archi review"

// reviewOutcome is what a finished `archi review -patch` run means.
type reviewOutcome struct {
	Exit               int // 0 clean/advisory, 1 blocking, 2 operational
	Findings, Blocking int // parsed from the doc footer
	Doc                string
}

// parseReviewFooter extracts "_N findings · M blocking · exit E_" from the
// last line of the review doc; an absent footer leaves zeros with Exit taken
// from the process. Pure.
func parseReviewFooter(doc string, processExit int) reviewOutcome {
	o := reviewOutcome{Exit: processExit, Doc: doc}
	trimmed := strings.TrimSpace(doc)
	if trimmed == "" {
		return o
	}
	all := strings.Split(trimmed, "\n")
	last := strings.TrimSpace(all[len(all)-1])
	var findings, blocking, exit int
	if _, err := fmt.Sscanf(last, "_%d findings · %d blocking · exit %d_", &findings, &blocking, &exit); err == nil {
		o.Findings, o.Blocking, o.Exit = findings, blocking, exit
	}
	return o
}

// conclusionFor maps an outcome to a check conclusion and title. An
// operational fault (exit 2, or the tool not running at all) is neutral —
// archi being down must not block merges.
func conclusionFor(o reviewOutcome, ranOK bool) (conclusion, title string) {
	if !ranOK || o.Exit == 2 {
		return "neutral", "archi review could not run"
	}
	switch {
	case o.Exit == 1:
		return "failure", fmt.Sprintf("archi review: %d blocking findings", o.Blocking)
	case o.Findings > 0:
		return "neutral", fmt.Sprintf("archi review: %d advisory findings", o.Findings)
	default:
		return "success", "archi review: clean"
	}
}

// checkTextMax bounds check-run output.text, under GitHub's 65535 limit.
const checkTextMax = 65000

// truncateCheckText clips s on a line boundary with a pointer to the full
// review in the PR thread. Pure.
func truncateCheckText(s string) string {
	if len(s) <= checkTextMax {
		return s
	}
	const trailer = "\n\n_…truncated — full review posted as a PR comment._"
	cut := s[:checkTextMax-len(trailer)]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + trailer
}
