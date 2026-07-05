package main

import (
	"fmt"
	"sort"
	"strings"
)

// langStat aggregates one language across the scanned repo.
type langStat struct {
	name   string
	files  int
	tokens int
}

// languageStats summarizes the scan by language, sorted by token weight desc.
func languageStats(files []fileInfo) (stats []langStat, totalFiles, totalTokens int) {
	byLang := map[string]*langStat{}
	for _, f := range files {
		name := f.lang
		if name == "" {
			name = "other"
		}
		s := byLang[name]
		if s == nil {
			s = &langStat{name: name}
			byLang[name] = s
		}
		s.files++
		s.tokens += estimateTokensN(f.size)
		totalFiles++
		totalTokens += estimateTokensN(f.size)
	}
	for _, s := range byLang {
		stats = append(stats, *s)
	}
	sortLangStats(stats)
	return stats, totalFiles, totalTokens
}

// sortLangStats orders languages by token weight desc, name as tiebreak.
func sortLangStats(stats []langStat) {
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].tokens != stats[j].tokens {
			return stats[i].tokens > stats[j].tokens
		}
		return stats[i].name < stats[j].name
	})
}

// buildMap renders the deterministic repo map cached as .archi/map.md and sent
// to the model as always-on grounding: totals, a language table, and a tree.
func buildMap(root string, files []fileInfo) string {
	stats, totalFiles, totalTokens := languageStats(files)

	var b strings.Builder
	name := root
	if name == "." || name == "" {
		name = "repository"
	}
	fmt.Fprintf(&b, "# Repo map: %s\n\n", name)
	fmt.Fprintf(&b, "%d files · ~%s tokens\n\n", totalFiles, humanCount(totalTokens))

	b.WriteString("| Language | Files | ~Tokens |\n|---|---|---|\n")
	for _, s := range stats {
		fmt.Fprintf(&b, "| %s | %d | %s |\n", s.name, s.files, humanCount(s.tokens))
	}

	b.WriteString("\n```\n")
	b.WriteString(renderTree(files))
	b.WriteString("```\n")
	return b.String()
}

// renderTree draws an ASCII directory tree, dirs before files, each file
// annotated with its language.
func renderTree(files []fileInfo) string {
	type node struct {
		children map[string]*node
		order    []string
		file     *fileInfo
	}
	root := &node{children: map[string]*node{}}

	for i := range files {
		cur := root
		parts := strings.Split(files[i].rel, "/")
		for j, p := range parts {
			child, ok := cur.children[p]
			if !ok {
				child = &node{children: map[string]*node{}}
				cur.children[p] = child
				cur.order = append(cur.order, p)
			}
			if j == len(parts)-1 {
				child.file = &files[i]
			}
			cur = child
		}
	}

	sortChildren := func(n *node) {
		sort.Slice(n.order, func(i, j int) bool {
			ci, cj := n.children[n.order[i]], n.children[n.order[j]]
			di, dj := len(ci.children) > 0, len(cj.children) > 0
			if di != dj {
				return di
			}
			return n.order[i] < n.order[j]
		})
	}

	var b strings.Builder
	var walk func(n *node, prefix string)
	walk = func(n *node, prefix string) {
		sortChildren(n)
		for i, nm := range n.order {
			child := n.children[nm]
			last := i == len(n.order)-1
			branch, indent := "├── ", "│   "
			if last {
				branch, indent = "└── ", "    "
			}
			label := nm
			if len(child.children) > 0 {
				label += "/"
			} else if child.file != nil && child.file.lang != "" {
				label = fmt.Sprintf("%s  · %s", nm, child.file.lang)
			}
			fmt.Fprintf(&b, "%s%s%s\n", prefix, branch, label)
			if len(child.children) > 0 {
				walk(child, prefix+indent)
			}
		}
	}
	walk(root, "")
	return b.String()
}
