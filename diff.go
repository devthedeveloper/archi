package main

import (
	"fmt"
	"strings"
)

// maxDiffLines is the per-side line cap beyond which unifiedDiff declines to
// run its quadratic comparison and returns a short notice instead.
const maxDiffLines = 5000

// diffContext is how many unchanged lines frame each hunk.
const diffContext = 3

// diffOp is one line of an edit script: kind is ' ' (equal), '-' (only in
// the old text), or '+' (only in the new text).
type diffOp struct {
	kind byte
	line string
}

// unifiedDiff returns a unified diff (with diffContext lines of context)
// between oldText and newText, labeled a/path and b/path. Identical inputs
// yield "". If either side exceeds maxDiffLines the comparison is skipped and
// a "file too large to diff" notice is returned instead.
func unifiedDiff(oldText, newText, path string) string {
	if oldText == newText {
		return ""
	}
	a, b := splitDiffLines(oldText), splitDiffLines(newText)
	if len(a) > maxDiffLines || len(b) > maxDiffLines {
		return fmt.Sprintf("--- a/%s\n+++ b/%s\n(file too large to diff)\n", path, path)
	}
	hunks := groupHunks(diffOps(a, b), diffContext)
	if len(hunks) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	for _, h := range hunks {
		sb.WriteString(h.header())
		for _, op := range h.ops {
			sb.WriteByte(op.kind)
			sb.WriteString(op.line)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// splitDiffLines splits text into lines for diffing; a trailing newline does
// not produce a phantom empty last line.
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffOps computes a line-level edit script turning a into b. Common prefix
// and suffix are peeled off first so the quadratic LCS only sees the changed
// middle, which for typical overwrites is small.
func diffOps(a, b []string) []diffOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	ops := make([]diffOp, 0, len(a)+len(b)-p-s)
	for _, l := range a[:p] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, lcsOps(a[p:len(a)-s], b[p:len(b)-s])...)
	for _, l := range a[len(a)-s:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

// lcsOps produces an edit script for two line slices via a longest-common-
// subsequence table. O(len(a)*len(b)) time and space, which maxDiffLines and
// the prefix/suffix peeling in diffOps keep affordable.
func lcsOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// lcs[i*(m+1)+j] = LCS length of a[i:] and b[j:].
	lcs := make([]int32, (n+1)*(m+1))
	at := func(i, j int) int32 { return lcs[i*(m+1)+j] }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i*(m+1)+j] = at(i+1, j+1) + 1
			case at(i+1, j) >= at(i, j+1):
				lcs[i*(m+1)+j] = at(i+1, j)
			default:
				lcs[i*(m+1)+j] = at(i, j+1)
			}
		}
	}
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case at(i+1, j) >= at(i, j+1):
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// hunk is a run of diff ops with its @@ range coordinates.
type hunk struct {
	aStart, aCount int
	bStart, bCount int
	ops            []diffOp
}

// header renders the hunk's @@ range line.
func (h hunk) header() string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", h.aStart, h.aCount, h.bStart, h.bCount)
}

// groupHunks folds an edit script into hunks, keeping ctx unchanged lines
// around each run of changes and merging runs closer than 2*ctx apart.
func groupHunks(ops []diffOp, ctx int) []hunk {
	// Line numbers (0-based) on each side just before ops[i].
	aAt := make([]int, len(ops)+1)
	bAt := make([]int, len(ops)+1)
	for i, op := range ops {
		aAt[i+1], bAt[i+1] = aAt[i], bAt[i]
		if op.kind != '+' {
			aAt[i+1]++
		}
		if op.kind != '-' {
			bAt[i+1]++
		}
	}

	var hunks []hunk
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i - ctx
		if start < 0 {
			start = 0
		}
		// Extend through changes separated by at most 2*ctx equal lines.
		end, run := i, 0
		for j := i; j < len(ops); j++ {
			if ops[j].kind == ' ' {
				if run++; run > 2*ctx {
					break
				}
			} else {
				run, end = 0, j
			}
		}
		stop := end + ctx + 1
		if stop > len(ops) {
			stop = len(ops)
		}
		h := hunk{aStart: aAt[start] + 1, bStart: bAt[start] + 1, ops: ops[start:stop]}
		for _, op := range h.ops {
			if op.kind != '+' {
				h.aCount++
			}
			if op.kind != '-' {
				h.bCount++
			}
		}
		if h.aCount == 0 {
			h.aStart = aAt[start]
		}
		if h.bCount == 0 {
			h.bStart = bAt[start]
		}
		hunks = append(hunks, h)
		i = stop
	}
	return hunks
}
