package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitignoreMatch(t *testing.T) {
	ig := parseGitignore(strings.NewReader("node_modules/\n/dist\n*.log\n!keep.log\nbuild/**/*.tmp\n"))
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"node_modules/react/index.js", false, true},
		{"src/node_modules/x.js", false, true},
		{"dist", true, true},
		{"dist/app.js", false, true},
		{"src/dist", true, false},
		{"app.log", false, true},
		{"logs/server.log", false, true},
		{"keep.log", false, false},
		{"build/a/b/x.tmp", false, true},
		{"build/x.go", false, false},
		{"main.go", false, false},
	}
	for _, c := range cases {
		if got := ig.Match(c.path, c.isDir); got != c.want {
			t.Errorf("Match(%q, %v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestScanRepoRespectsGitignoreAndBinaries(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "ignored/\n*.log\n")
	write("main.go", "package main\n")
	write("ignored/secret.go", "package x\n")
	write("debug.log", "noise\n")
	write("sub/util.py", "print(1)\n")

	files, err := scanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.rel] = f.lang
	}
	if _, ok := got["main.go"]; !ok || got["main.go"] != "Go" {
		t.Errorf("expected main.go as Go, got %v", got)
	}
	if got["sub/util.py"] != "Python" {
		t.Errorf("expected sub/util.py as Python, got %v", got)
	}
	if _, ok := got["ignored/secret.go"]; ok {
		t.Error("ignored/ should be excluded")
	}
	if _, ok := got["debug.log"]; ok {
		t.Error("*.log should be excluded")
	}
}

func TestSelectSeedPrioritizesManifests(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	write("go.mod", "module x\n")
	write("README.md", "# X\n")
	write("main.go", "package main\n")
	write("internal/deep/helper.go", "package deep\n")

	files, err := scanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed, used := selectSeed(dir, files, 40000, 256*1024)
	if used == 0 || len(seed) == 0 {
		t.Fatal("expected a non-empty seed")
	}
	// go.mod and README come before the deep helper.
	order := map[string]int{}
	for i, s := range seed {
		order[s.rel] = i
	}
	if order["go.mod"] > order["internal/deep/helper.go"] {
		t.Error("go.mod should be selected before deep source")
	}
	if order["README.md"] > order["internal/deep/helper.go"] {
		t.Error("README should be selected before deep source")
	}
}

func TestResolveInputPrecedence(t *testing.T) {
	// args win
	if got, _ := resolveInput([]string{"add", "auth"}, []byte("file"), true, strings.NewReader("stdin"), false); got != "add auth" {
		t.Errorf("args should win, got %q", got)
	}
	// file beats stdin
	if got, _ := resolveInput(nil, []byte("from file"), true, strings.NewReader("stdin"), false); got != "from file" {
		t.Errorf("file should win over stdin, got %q", got)
	}
	// stdin when piped
	if got, _ := resolveInput(nil, nil, false, strings.NewReader("piped in"), false); got != "piped in" {
		t.Errorf("stdin should be used, got %q", got)
	}
	// nothing, interactive stdin -> error
	if _, err := resolveInput(nil, nil, false, strings.NewReader(""), true); err == nil {
		t.Error("expected error when no input given")
	}
}

func TestNewProvider(t *testing.T) {
	p, err := newProvider("ollama", "", 0.4, 8000)
	if err != nil || p.Model() != defaultOllamaModel {
		t.Errorf("ollama default = %v, %v", p, err)
	}
	if _, ok := p.(*ollamaProvider); !ok {
		t.Error("expected *ollamaProvider")
	}
	a, err := newProvider("anthropic", "claude-x", 0.4, 8000)
	if err != nil || a.Model() != "claude-x" {
		t.Errorf("anthropic model = %v, %v", a, err)
	}
	if _, err := newProvider("bogus", "", 0.4, 8000); err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestParseOllamaLine(t *testing.T) {
	text, done, err := parseOllamaLine([]byte(`{"message":{"content":"hello"},"done":false}`))
	if err != nil || text != "hello" || done {
		t.Errorf("got %q done=%v err=%v", text, done, err)
	}
	_, done, _ = parseOllamaLine([]byte(`{"message":{"content":""},"done":true}`))
	if !done {
		t.Error("expected done=true")
	}
	if _, _, err := parseOllamaLine([]byte(`{"error":"boom"}`)); err == nil {
		t.Error("expected error surfaced")
	}
}

func TestParseAnthropicSSE(t *testing.T) {
	text, stop := parseAnthropicSSE(`data: {"type":"content_block_delta","delta":{"text":"hi"}}`)
	if text != "hi" || stop {
		t.Errorf("got %q stop=%v", text, stop)
	}
	if _, stop := parseAnthropicSSE(`data: {"type":"message_stop"}`); !stop {
		t.Error("expected stop=true")
	}
	if text, _ := parseAnthropicSSE(`event: ping`); text != "" {
		t.Error("non-data line should yield no text")
	}
}

func TestDesignUserMessageStructure(t *testing.T) {
	msg := designUserMessage("PROFILE-TEXT", "MAP-TEXT", nil, "add caching")
	for _, want := range []string{"CODEBASE PROFILE", "FILE MAP", "CHANGE REQUEST", "PROFILE-TEXT", "add caching"} {
		if !strings.Contains(msg, want) {
			t.Errorf("design message missing %q", want)
		}
	}
}

func TestParseQuestions(t *testing.T) {
	in := `Q: How many orders per second at peak?
Why: above ~5k/s we need sharding, below it a single Postgres is fine
Options: <1k, 1k-5k, >5k
---
Q: Must delivery be exactly-once?
Why: exactly-once needs an outbox; at-least-once is simpler
Options: exactly-once, at-least-once
---
garbage block with no question`
	qs := parseQuestions(in)
	if len(qs) != 2 {
		t.Fatalf("expected 2 questions, got %d", len(qs))
	}
	if !strings.Contains(qs[0].Q, "orders per second") || qs[0].Options != "<1k, 1k-5k, >5k" {
		t.Errorf("first question parsed wrong: %+v", qs[0])
	}
	if qs[1].Why == "" {
		t.Error("expected a Why on the second question")
	}
}

func TestParseFileBlocks(t *testing.T) {
	in := "here is noise\n" +
		"=== FILE: internal/cache/cache.go ===\n" +
		"```go\n" +
		"package cache\n\nfunc Get() {}\n" +
		"```\n" +
		"=== DELETE: old/legacy.go ===\n" +
		"trailing chatter"
	fcs := parseFileBlocks(in)
	if len(fcs) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(fcs))
	}
	if fcs[0].path != "internal/cache/cache.go" || !strings.Contains(fcs[0].content, "package cache") {
		t.Errorf("file block parsed wrong: %+v", fcs[0])
	}
	if fcs[0].del {
		t.Error("first block should not be a delete")
	}
	if fcs[1].path != "old/legacy.go" || !fcs[1].del {
		t.Errorf("delete block parsed wrong: %+v", fcs[1])
	}
}

func TestCodeStreamRender(t *testing.T) {
	var buf bytes.Buffer
	cs := &codeStreamWriter{w: &buf}
	// Feed the stream in two chunks, splitting a line mid-way, to exercise buffering.
	cs.Write([]byte("=== FILE: internal/x.go ===\n```go\npack"))
	cs.Write([]byte("age x\nfunc F() {}\n```\n=== DELETE: old.go ===\n"))
	cs.flush()
	out := buf.String()
	if !strings.Contains(out, "internal/x.go") {
		t.Errorf("missing file header: %q", out)
	}
	if !strings.Contains(out, "package x") || !strings.Contains(out, "func F() {}") {
		t.Errorf("missing streamed code: %q", out)
	}
	if !strings.Contains(out, "delete old.go") {
		t.Errorf("missing delete line: %q", out)
	}
	if strings.Contains(out, "```") {
		t.Errorf("fence lines should be dropped: %q", out)
	}
}

func TestSafeRel(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"internal/x.go", true},
		{"x.go", true},
		{"../escape.go", false},
		{"/etc/passwd", false},
		{"a/../../b.go", false},
		{"", false},
	}
	for _, c := range cases {
		if _, ok := safeRel(c.in); ok != c.want {
			t.Errorf("safeRel(%q) ok = %v, want %v", c.in, ok, c.want)
		}
	}
}
