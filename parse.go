package main

import "strings"

// question is one clarifying question the model asks before designing.
type question struct {
	Q       string
	Why     string
	Options string
}

// parseQuestions extracts Q/Why/Options blocks (separated by "---") from the
// clarify response. Malformed blocks without a question are dropped.
func parseQuestions(s string) []question {
	var qs []question
	for _, block := range strings.Split(s, "---") {
		var q question
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "Q:"):
				q.Q = strings.TrimSpace(line[len("Q:"):])
			case strings.HasPrefix(line, "Why:"):
				q.Why = strings.TrimSpace(line[len("Why:"):])
			case strings.HasPrefix(line, "Options:"):
				q.Options = strings.TrimSpace(line[len("Options:"):])
			}
		}
		if q.Q != "" {
			qs = append(qs, q)
		}
	}
	return qs
}

// fileChange is one file the build phase will create, replace, or delete.
type fileChange struct {
	path    string
	content string
	del     bool
}

// parseFileBlocks extracts "=== FILE: path ===" (with a fenced body) and
// "=== DELETE: path ===" markers from the build response.
func parseFileBlocks(s string) []fileChange {
	var out []fileChange
	lines := strings.Split(s, "\n")
	i := 0
	for i < len(lines) {
		line := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(line, "=== DELETE:"):
			p := trimMarker(line, "=== DELETE:")
			if p != "" {
				out = append(out, fileChange{path: p, del: true})
			}
			i++
		case strings.HasPrefix(line, "=== FILE:"):
			p := trimMarker(line, "=== FILE:")
			i++
			// Skip an opening code fence if present.
			if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				i++
			}
			var body []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				body = append(body, lines[i])
				i++
			}
			if i < len(lines) { // skip closing fence
				i++
			}
			if p != "" {
				out = append(out, fileChange{path: p, content: strings.Join(body, "\n")})
			}
		default:
			i++
		}
	}
	return out
}

// trimMarker strips a "=== LABEL:" prefix and trailing "=" / spaces from a path.
func trimMarker(line, prefix string) string {
	return strings.Trim(strings.TrimPrefix(line, prefix), " =")
}
