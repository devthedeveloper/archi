package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// codeStreamWriter renders the build phase's streamed output as a live,
// readable view: each "=== FILE: path ===" marker becomes a highlighted header
// and the code beneath it streams dimmed. Fence lines are dropped as noise. It
// buffers partial chunks and renders whole lines as they complete.
type codeStreamWriter struct {
	w   io.Writer
	buf []byte
}

func newCodeStream() *codeStreamWriter { return &codeStreamWriter{w: os.Stderr} }

func (c *codeStreamWriter) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			break
		}
		c.render(string(c.buf[:i]))
		c.buf = c.buf[i+1:]
	}
	return len(p), nil
}

// flush renders any trailing partial line at the end of the stream.
func (c *codeStreamWriter) flush() {
	if len(c.buf) > 0 {
		c.render(string(c.buf))
		c.buf = nil
	}
}

func (c *codeStreamWriter) render(line string) {
	t := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(t, "=== FILE:"):
		fmt.Fprintln(c.w, "  "+green("›")+" "+bold(trimMarker(t, "=== FILE:")))
	case strings.HasPrefix(t, "=== DELETE:"):
		fmt.Fprintln(c.w, "  "+red("›")+" "+dim("delete ")+trimMarker(t, "=== DELETE:"))
	case strings.HasPrefix(t, "```"):
		// drop code-fence lines
	default:
		fmt.Fprintln(c.w, "    "+dim(line))
	}
}
