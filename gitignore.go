package main

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

// gitignore holds an ordered set of compiled .gitignore patterns. Precedence
// follows git: the last pattern to match a path wins, so a later "!keep.log"
// can re-include something an earlier "*.log" excluded.
type gitignore struct {
	patterns []pattern
}

type pattern struct {
	negate  bool
	dirOnly bool
	re      *regexp.Regexp
}

// parseGitignore reads gitignore-syntax lines: comments, blanks, "!" negation,
// trailing "/" (directory-only), leading "/" (anchored), and *, **, ? wildcards.
func parseGitignore(r io.Reader) *gitignore {
	g := &gitignore{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := pattern{}
		if strings.HasPrefix(line, "!") {
			p.negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		anchored := strings.HasPrefix(line, "/") || strings.Contains(strings.TrimSuffix(line, "/"), "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		body := globToRegex(line)
		expr := "^" + body + "$"
		if !anchored {
			expr = "^(?:.*/)?" + body + "$"
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			continue
		}
		p.re = re
		g.patterns = append(g.patterns, p)
	}
	return g
}

// globToRegex converts one gitignore glob into a regex body (no anchors).
func globToRegex(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ignoreChain layers scoped .gitignore files — the root one plus any found in
// subdirectories — following git semantics: a file's patterns only apply to
// paths under its own directory, matched relative to that directory, and the
// last matching pattern (deeper files come later) wins.
type ignoreChain struct {
	scopes []scopedIgnore
}

// scopedIgnore is one parsed .gitignore bound to the directory it lives in.
type scopedIgnore struct {
	dir string // slash-separated dir relative to root; "" for the root file
	ig  *gitignore
}

// add registers a parsed .gitignore scoped to dir ("" for the repo root).
// Callers must add parents before children so precedence follows git.
func (c *ignoreChain) add(dir string, ig *gitignore) {
	if ig == nil || len(ig.patterns) == 0 {
		return
	}
	c.scopes = append(c.scopes, scopedIgnore{dir: dir, ig: ig})
}

// Match reports whether relPath (slash-separated, relative to root) is ignored
// by any .gitignore in the chain that governs it.
func (c *ignoreChain) Match(relPath string, isDir bool) bool {
	ignored := false
	for _, s := range c.scopes {
		sub := relPath
		if s.dir != "" {
			if !strings.HasPrefix(relPath, s.dir+"/") {
				continue // this .gitignore doesn't govern relPath
			}
			sub = relPath[len(s.dir)+1:]
		}
		for _, p := range s.ig.patterns {
			if p.matches(sub, isDir) {
				ignored = !p.negate
			}
		}
	}
	return ignored
}

// Match reports whether relPath (slash-separated, no trailing slash) is ignored.
func (g *gitignore) Match(relPath string, isDir bool) bool {
	if len(g.patterns) == 0 {
		return false
	}
	ignored := false
	for _, p := range g.patterns {
		if p.matches(relPath, isDir) {
			ignored = !p.negate
		}
	}
	return ignored
}

// matches tests the pattern against the path and each ancestor directory, so
// excluding "build/" also excludes "build/app/main.go".
func (p pattern) matches(relPath string, isDir bool) bool {
	if p.re.MatchString(relPath) && (!p.dirOnly || isDir) {
		return true
	}
	for i := 0; i < len(relPath); i++ {
		if relPath[i] == '/' && p.re.MatchString(relPath[:i]) {
			return true
		}
	}
	return false
}
