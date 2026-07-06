package main

import (
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The context budget caps how much grounding (profile + map + focus files) a
// design request carries, so a huge repo or a greedy -focus glob can't blow
// past the model's context window. estimateTokens (util.go) supplies the
// ~4-chars-per-token estimate; the fitting itself lives in small pure
// functions here so it's testable.

// defaultContextTokens is the grounding budget when ARCHI_CONTEXT_TOKENS is
// unset — roomy enough for a full profile and map plus several focus files.
const defaultContextTokens = 48000

// minPartTokens is the smallest truncated slice worth sending; below this a
// part is dropped rather than reduced to a useless stub.
const minPartTokens = 200

// truncatedMark is appended to any part that was cut to fit the budget.
const truncatedMark = "\n\n… truncated to fit the context budget"

// contextBudget resolves the grounding budget, honoring ARCHI_CONTEXT_TOKENS.
func contextBudget() int {
	if v := strings.TrimSpace(os.Getenv("ARCHI_CONTEXT_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		warn("ignoring invalid ARCHI_CONTEXT_TOKENS=" + v)
	}
	return defaultContextTokens
}

// budgetPart is one piece of prompt grounding competing for the budget.
type budgetPart struct {
	name     string
	required bool // always included whole, even past the budget
	content  string
}

// budgetNote records what fitBudget did to a part, for user-facing warnings.
type budgetNote struct {
	name   string
	action string // "truncated" or "dropped"
}

// fitBudget fits parts into a token budget, preserving order. Required parts
// are always kept whole (they alone may exceed the budget). Every other part
// is kept whole when it fits the remaining room; when one doesn't, it keeps
// its head with a truncation marker if at least minPartTokens of room remain,
// and is dropped entirely otherwise. Notes name each part that was cut.
func fitBudget(parts []budgetPart, budget int) ([]budgetPart, []budgetNote) {
	used := 0
	for _, p := range parts {
		if p.required {
			used += estimateTokens(p.content)
		}
	}
	var out []budgetPart
	var notes []budgetNote
	for _, p := range parts {
		if p.required {
			out = append(out, p)
			continue
		}
		room := budget - used
		switch t := estimateTokens(p.content); {
		case t <= room:
			used += t
			out = append(out, p)
		case room >= minPartTokens:
			p.content = truncateToTokens(p.content, room)
			used += estimateTokens(p.content)
			out = append(out, p)
			notes = append(notes, budgetNote{name: p.name, action: "truncated"})
		default:
			notes = append(notes, budgetNote{name: p.name, action: "dropped"})
		}
	}
	return out, notes
}

// truncateToTokens keeps the head of s that fits in roughly max tokens,
// cutting at a line boundary where possible and appending truncatedMark.
func truncateToTokens(s string, max int) string {
	keep := max*4 - len(truncatedMark)
	if keep >= len(s) {
		return s
	}
	if keep < 0 {
		keep = 0
	}
	cut := s[:keep]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1] // don't leave a split rune at the boundary
	}
	return cut + truncatedMark
}

// fitGrounding applies the context budget to a design request's grounding:
// the profile always ships whole, the map and focus files shrink or drop to
// fit, with a warning naming everything that was cut.
func fitGrounding(profile, mapMD string, focus []seedFile) (string, string, []seedFile) {
	parts := []budgetPart{
		{name: "profile", required: true, content: profile},
		{name: "file map", content: mapMD},
	}
	for _, f := range focus {
		parts = append(parts, budgetPart{name: f.rel, content: f.content})
	}
	fitted, notes := fitBudget(parts, contextBudget())
	if len(notes) == 0 {
		return profile, mapMD, focus
	}
	for _, n := range notes {
		warn("context budget: " + n.action + " " + n.name + dim("  ·  raise ARCHI_CONTEXT_TOKENS to include more"))
	}
	byRel := make(map[string]seedFile, len(focus))
	for _, f := range focus {
		byRel[f.rel] = f
	}
	outMap := ""
	var outFocus []seedFile
	for _, p := range fitted {
		switch p.name {
		case "profile":
			// required: always whole
		case "file map":
			outMap = p.content
		default:
			sf := byRel[p.name]
			sf.content = p.content
			outFocus = append(outFocus, sf)
		}
	}
	return profile, outMap, outFocus
}
