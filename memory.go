package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Memory is one append-only Markdown file of ADR-ish entries under .archi/.
// Like design history it is advisory: every failure warns and continues.

func memoryDir(root string) string  { return filepath.Join(archiDir(root), "memory") }
func memoryPath(root string) string { return filepath.Join(memoryDir(root), "decisions.md") }

// defaultMemoryTokens caps memory's share of the design grounding budget.
const defaultMemoryTokens = 4000

// memoryBudget resolves ARCHI_MEMORY_TOKENS, else the default.
func memoryBudget() int {
	if v := strings.TrimSpace(os.Getenv("ARCHI_MEMORY_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		warn("ignoring invalid ARCHI_MEMORY_TOKENS=" + v)
	}
	return defaultMemoryTokens
}

// loadMemory returns the raw memory file, "" when absent or unreadable.
func loadMemory(root string) string {
	data, err := os.ReadFile(memoryPath(root))
	if err != nil {
		return ""
	}
	return string(data)
}

// memoryEntry is one "## …" section.
type memoryEntry struct {
	heading string
	body    string
}

// splitMemoryEntries splits on lines beginning "## ". Preamble is dropped.
func splitMemoryEntries(s string) []memoryEntry {
	lines := strings.Split(s, "\n")
	i := 0
	for i < len(lines) && !strings.HasPrefix(lines[i], "## ") {
		i++
	}
	var entries []memoryEntry
	for i < len(lines) {
		heading := lines[i]
		i++
		var body []string
		for i < len(lines) && !strings.HasPrefix(lines[i], "## ") {
			body = append(body, lines[i])
			i++
		}
		entries = append(entries, memoryEntry{heading: heading, body: strings.Trim(strings.Join(body, "\n"), "\n")})
	}
	return entries
}

// fitMemory selects the newest whole entries that fit the budget, emitted
// newest first under a header, with an omission trailer when some were dropped.
func fitMemory(s string, budget int) string {
	entries := splitMemoryEntries(s)
	if len(entries) == 0 {
		return ""
	}
	var selected []memoryEntry
	used, omitted := 0, 0
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		t := estimateTokens(e.heading + "\n" + e.body)
		if used+t > budget && len(selected) > 0 {
			omitted = i + 1
			break
		}
		selected = append(selected, e)
		used += t
	}
	if len(selected) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Team memory (newest first)\n")
	for _, e := range selected {
		b.WriteString("\n" + e.heading + "\n" + e.body + "\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n… %d older entries omitted (archi memory show)\n", omitted)
	}
	return b.String()
}

// factLines collects every "- " bullet line (trimmed), for dedup.
func factLines(s string) map[string]bool {
	m := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- ") {
			m[t] = true
		}
	}
	return m
}

// dedupEntryFacts drops bullet lines whose trimmed text already exists.
func dedupEntryFacts(entry string, existing map[string]bool) string {
	var out []string
	for _, line := range strings.Split(entry, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "- ") && existing[t] {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// appendMemoryEntry dedups the entry's facts, prepends the dated heading, and
// appends it to the memory file.
func appendMemoryEntry(root, request string, t time.Time, body string) error {
	body = dedupEntryFacts(body, factLines(loadMemory(root)))
	heading := "## " + t.Format("2006-01-02") + " " + designSlug(request)
	if err := os.MkdirAll(memoryDir(root), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(memoryPath(root), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + heading + "\n\n" + strings.TrimSpace(body) + "\n")
	return err
}

// memoryStats returns the entry count and the date token of the last heading.
func memoryStats(root string) (int, string) {
	entries := splitMemoryEntries(loadMemory(root))
	if len(entries) == 0 {
		return 0, ""
	}
	fields := strings.Fields(strings.TrimPrefix(entries[len(entries)-1].heading, "## "))
	date := ""
	if len(fields) > 0 {
		date = fields[0]
	}
	return len(entries), date
}

// recordMemory summarizes a completed build into a durable memory entry.
// Best-effort: any error warns and returns, never failing the build flow.
func recordMemory(ctx context.Context, mk providerFactory, root, request, interview, design, outcome string) {
	sp := startSpinner("Remembering this decision")
	out, err := runModel(ctx, mk(1200), memorySummarizerPrompt, memorySummaryUserMessage(design, interview, outcome), nil, sp)
	if err != nil {
		sp.Abort()
		warn("could not summarize decision: " + err.Error())
		return
	}
	sp.Stop("Decision remembered")
	if !strings.Contains(out, "**Decided:**") {
		warn("summarizer returned no usable entry — skipped")
		return
	}
	if err := appendMemoryEntry(root, request, time.Now(), out); err != nil {
		warn("could not save memory: " + err.Error())
	}
}

// validCompactedMemory guards against a degenerate rewrite: non-empty, has a
// heading, and keeps ≥25% of the original entry count.
func validCompactedMemory(original, rewritten string) bool {
	if strings.TrimSpace(rewritten) == "" || !strings.Contains(rewritten, "## ") {
		return false
	}
	origN := len(splitMemoryEntries(original))
	newN := len(splitMemoryEntries(rewritten))
	return origN == 0 || newN*4 >= origN
}
