package main

import (
	"bytes"
	"fmt"
	"os"
)

// estimateTokens approximates LLM tokens from text using the common ~4-chars
// per token rule of thumb. It's a budgeting estimate, not an exact count.
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

// estimateTokensN estimates tokens from a byte length (used for the file map,
// where reading every file just to count would be wasteful).
func estimateTokensN(n int64) int { return int((n + 3) / 4) }

// humanCount formats 1234 -> "1.2k", 2_000_000 -> "2.0M".
func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// humanBytes formats 1536 -> "1.5 KB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// isBinary reports whether data looks non-textual — a NUL byte in the first
// 8 KB, the same heuristic git uses.
func isBinary(data []byte) bool {
	if len(data) > 8000 {
		data = data[:8000]
	}
	return bytes.IndexByte(data, 0) != -1
}

// isTTY reports whether f is an interactive terminal (not a pipe or file).
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// funcWriter adapts a function into an io.Writer.
type funcWriter func([]byte) (int, error)

func (f funcWriter) Write(b []byte) (int, error) { return f(b) }
