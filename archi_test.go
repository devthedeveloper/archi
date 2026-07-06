package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	o, err := newProvider("openai", "", 0.4, 8000)
	if err != nil || o.Model() != defaultOpenAIModel {
		t.Errorf("openai default = %v, %v", o, err)
	}
	if _, ok := o.(*openaiProvider); !ok {
		t.Error("expected *openaiProvider")
	}
	if o.Name() != "openai/"+defaultOpenAIModel {
		t.Errorf("openai name = %q", o.Name())
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

func TestParseOpenAISSE(t *testing.T) {
	cases := []struct {
		line string
		text string
		stop bool
	}{
		{`data: {"choices":[{"delta":{"content":"hi"}}]}`, "hi", false},
		{`data:{"choices":[{"delta":{"content":" there"}}]}`, " there", false},
		{`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, "", false},
		{`data: [DONE]`, "", true},
		{`data:`, "", false},
		{`event: ping`, "", false},
		{``, "", false},
		{`data: not-json`, "", false},
		{`data: {"choices":[]}`, "", false},
	}
	for _, c := range cases {
		text, stop := parseOpenAISSE(c.line)
		if text != c.text || stop != c.stop {
			t.Errorf("parseOpenAISSE(%q) = (%q, %v), want (%q, %v)", c.line, text, stop, c.text, c.stop)
		}
	}
}

func TestIsRetryableStatus(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{200, false}, {201, false}, {400, false}, {401, false}, {403, false},
		{404, false}, {429, true}, {500, true}, {502, true}, {503, true}, {529, true},
	}
	for _, c := range cases {
		if got := isRetryableStatus(c.code); got != c.want {
			t.Errorf("isRetryableStatus(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestIsRetryableErr(t *testing.T) {
	if isRetryableErr(context.Canceled) {
		t.Error("context.Canceled should not be retryable")
	}
	if isRetryableErr(fmt.Errorf("do: %w", context.DeadlineExceeded)) {
		t.Error("wrapped deadline exceeded should not be retryable")
	}
	if !isRetryableErr(fmt.Errorf("connection reset by peer")) {
		t.Error("a generic transport error should be retryable")
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"0", 0, true},
		{"2", 2 * time.Second, true},
		{" 5 ", 5 * time.Second, true},
		{"120", retryAfterCap, true}, // capped at ~30s
		{"-1", 0, false},
		{"soon", 0, false},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0, false},
	}
	for _, c := range cases {
		got, ok := parseRetryAfter(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	// A usable Retry-After wins over backoff.
	if d := retryDelay(0, "3"); d != 3*time.Second {
		t.Errorf("retryDelay with Retry-After = %v, want 3s", d)
	}
	// Without one, backoff is jittered around 1s, 2s, 4s: within [d/2, 3d/2).
	for attempt := 0; attempt < 3; attempt++ {
		base := retryBase << attempt
		for i := 0; i < 20; i++ {
			got := retryDelay(attempt, "garbage")
			if got < base/2 || got >= base+base/2 {
				t.Fatalf("retryDelay(%d) = %v, want in [%v, %v)", attempt, got, base/2, base+base/2)
			}
		}
	}
}

func TestDoWithRetryGivesUpAfterMax(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	resp, err := doWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("final status = %d, want 503", resp.StatusCode)
	}
	if got := calls.Load(); got != maxRetries+1 {
		t.Errorf("attempts = %d, want %d", got, maxRetries+1)
	}
}

func TestDoWithRetryDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	resp, err := doWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestRequestContext(t *testing.T) {
	t.Setenv("ARCHI_TIMEOUT", "")
	ctx, cancel, err := requestContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("unset ARCHI_TIMEOUT should mean no deadline")
	}

	t.Setenv("ARCHI_TIMEOUT", "10m")
	ctx, cancel, err = requestContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Error("ARCHI_TIMEOUT=10m should set a deadline")
	}

	t.Setenv("ARCHI_TIMEOUT", "bogus")
	if _, _, err := requestContext(context.Background()); err == nil {
		t.Error("expected error for invalid ARCHI_TIMEOUT")
	}
}

func TestOpenAICompleteRetriesThenStreams(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\", world\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ARCHI_TIMEOUT", "30s")

	p, err := newProvider("openai", "", 0.2, 128)
	if err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	out, err := p.Complete(context.Background(), "sys", "user", &progress)
	if err != nil {
		t.Fatal(err)
	}
	if out != "Hello, world" {
		t.Errorf("out = %q, want %q", out, "Hello, world")
	}
	if progress.String() != out {
		t.Errorf("progress = %q, want %q", progress.String(), out)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (one 429, one success)", got)
	}
}

func TestDesignUserMessageStructure(t *testing.T) {
	msg := designUserMessage("PROFILE-TEXT", "MAP-TEXT", "", "", nil, "add caching")
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

func TestIgnoreChainScoping(t *testing.T) {
	chain := &ignoreChain{}
	chain.add("", parseGitignore(strings.NewReader("*.log\n!keep.log\n")))
	chain.add("sub", parseGitignore(strings.NewReader("build/\n*.tmp\n!keep.tmp\n!special.log\n")))
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"app.log", false, true},          // root pattern at root
		{"sub/deep.log", false, true},     // root pattern applies below sub
		{"keep.log", false, false},        // root negation
		{"sub/special.log", false, false}, // deeper negation overrides root
		{"sub/build", true, true},         // sub pattern under sub
		{"sub/build/gen.go", false, true}, // ...and everything inside it
		{"build", true, false},            // sub pattern does NOT apply at root
		{"sub/x.tmp", false, true},        // sub pattern, direct child
		{"sub/nested/y.tmp", false, true}, // sub pattern recurses within sub
		{"sub/keep.tmp", false, false},    // sub-local negation
		{"other/x.tmp", false, false},     // sub pattern scoped away from other/
		{"main.go", false, false},
	}
	for _, c := range cases {
		if got := chain.Match(c.path, c.isDir); got != c.want {
			t.Errorf("chain.Match(%q, %v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestScanRepoNestedGitignore(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "*.log\n")
	write("sub/.gitignore", "vendor/\n*.gen.go\n")
	write("main.go", "package main\n")
	write("app.log", "noise\n")
	write("sub/app.log", "noise\n")
	write("sub/vendor/lib.go", "package lib\n")
	write("sub/api.gen.go", "package sub\n")
	write("sub/real.go", "package sub\n")
	write("vendor/keep.go", "package vendor\n")
	write("other/api.gen.go", "package other\n")

	files, err := scanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.rel] = true
	}
	for _, want := range []string{"main.go", "sub/real.go", "vendor/keep.go", "other/api.gen.go"} {
		if !got[want] {
			t.Errorf("expected %s in scan results %v", want, got)
		}
	}
	for _, skip := range []string{"app.log", "sub/app.log", "sub/vendor/lib.go", "sub/api.gen.go"} {
		if got[skip] {
			t.Errorf("expected %s to be excluded, scan results %v", skip, got)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	pemBlock := "cert:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA7bq\nQIDAQAB\n-----END RSA PRIVATE KEY-----\ndone"
	cases := []struct {
		name string
		in   string
		want int
		gone string // must not survive redaction
		keep string // must survive redaction
	}{
		{"aws access key id", `key = "AKIAIOSFODNN7EXAMPLE"`, 1, "AKIAIOSFODNN7EXAMPLE", `key = "`},
		{"aws secret key", "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", 1, "wJalrXUtnFEMI", "aws_secret_access_key"},
		{"github classic token", "url = https://ghp_AbCdEfGhIjKlMnOpQrStUvWxYz1234@github.com", 1, "ghp_AbCdEfGh", "github.com"},
		{"github fine-grained", "token: github_pat_11ABCDEFG0abcdefghijklmnop", 1, "github_pat_11ABCDEFG0", "token:"},
		{"slack token", "SLACK_HOOK=xoxb-1234567890-abcdefghijkl", 1, "xoxb-1234567890", "SLACK_HOOK"},
		{"stripe live key", `stripe := "sk_live_abcdefghijklmnop1234"`, 1, "sk_live_", "stripe"},
		{"private key block", pemBlock, 1, "MIIEowIBAAKCAQEA7bq", "cert:"},
		{"generic api key", `api_key: "3f9c2b8a7d6e5f4a1b2c"`, 1, "3f9c2b8a7d6e5f4a1b2c", `api_key: "`},
		{"password assignment", "password = hunter2hunter2hunter2", 1, "hunter2hunter2hunter2", "password ="},
		{"go token assignment", `authToken := "eyJhbGciOiJIUzI1NiJ9abc"`, 1, "eyJhbGciOiJIUzI1NiJ9abc", "authToken"},
		{"bearer header", `req.Header.Set("Authorization", "Bearer abc123def456ghi789jkl")`, 1, "abc123def456ghi789jkl", "Authorization"},
		{"token in identifier survives", "tokenCount := estimateTokens(profile)\nfunc parseTokenStream() {}", 0, "", "tokenCount := estimateTokens(profile)"},
		{"short value survives", `token := "abc123"`, 0, "", `token := "abc123"`},
		{"prose survives", "The token budget for the seed selector is 40000 tokens.", 0, "", "token budget"},
	}
	for _, c := range cases {
		got, n := redactSecrets(c.in)
		if n != c.want {
			t.Errorf("%s: redactSecrets count = %d, want %d (output %q)", c.name, n, c.want, got)
		}
		if c.gone != "" && strings.Contains(got, c.gone) {
			t.Errorf("%s: secret %q survived: %q", c.name, c.gone, got)
		}
		if !strings.Contains(got, c.keep) {
			t.Errorf("%s: expected %q to survive, got %q", c.name, c.keep, got)
		}
		if c.want > 0 && !strings.Contains(got, redactedMark) {
			t.Errorf("%s: expected %s marker in %q", c.name, redactedMark, got)
		}
	}
}

func TestIsSecretFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".env", true},
		{"config/.env.production", true},
		{"certs/server.pem", true},
		{"certs/private.key", true},
		{"home/id_rsa", true},
		{"home/id_rsa.pub", true},
		{"main.go", false},
		{"keyboard.go", false},
		{"docs/env.md", false},
	}
	for _, c := range cases {
		if got := isSecretFile(c.path); got != c.want {
			t.Errorf("isSecretFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestScanRepoSkipsSecretFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	write("server.pem", "-----BEGIN CERTIFICATE-----\n")
	write("certs/private.key", "-----BEGIN RSA PRIVATE KEY-----\n")
	write("id_rsa", "-----BEGIN OPENSSH PRIVATE KEY-----\n")

	files, err := scanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].rel != "main.go" {
		t.Errorf("expected only main.go to survive, got %v", files)
	}
}

func TestUnifiedDiff(t *testing.T) {
	big := strings.Repeat("line\n", maxDiffLines+1)
	twenty := func(mut map[int]string) string {
		var b strings.Builder
		for i := 1; i <= 20; i++ {
			l := fmt.Sprintf("l%d", i)
			if m, ok := mut[i]; ok {
				l = m
			}
			b.WriteString(l + "\n")
		}
		return b.String()
	}
	cases := []struct {
		name     string
		old, new string
		hunks    int      // expected number of @@ headers
		contains []string // fragments that must appear
	}{
		{"identical", "a\nb\n", "a\nb\n", 0, nil},
		{"insert", "a\nb\n", "a\nx\nb\n", 1, []string{"+x", " a", " b"}},
		{"delete", "a\nx\nb\n", "a\nb\n", 1, []string{"-x", " a"}},
		{"modify", "a\nb\nc\n", "a\nx\nc\n", 1, []string{"-b", "+x", "@@ -1,3 +1,3 @@"}},
		{"insert into empty", "", "a\n", 1, []string{"@@ -0,0 +1,1 @@", "+a"}},
		{"far-apart changes get two hunks", twenty(nil), twenty(map[int]string{2: "X", 18: "Y"}), 2, []string{"-l2", "+X", "-l18", "+Y"}},
		{"size-cap fallback", big, "other\n", 0, []string{"file too large to diff"}},
	}
	for _, c := range cases {
		got := unifiedDiff(c.old, c.new, "f.txt")
		if c.old == c.new && got != "" {
			t.Errorf("%s: expected empty diff, got %q", c.name, got)
		}
		if n := strings.Count(got, "@@ -"); n != c.hunks {
			t.Errorf("%s: hunks = %d, want %d (diff %q)", c.name, n, c.hunks, got)
		}
		if got != "" && !strings.HasPrefix(got, "--- a/f.txt\n+++ b/f.txt\n") {
			t.Errorf("%s: missing header: %q", c.name, got)
		}
		for _, want := range c.contains {
			if !strings.Contains(got, want) {
				t.Errorf("%s: diff missing %q: %q", c.name, want, got)
			}
		}
	}

	want := "--- a/f.txt\n+++ b/f.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+x\n c\n"
	if got := unifiedDiff("a\nb\nc\n", "a\nx\nc\n", "f.txt"); got != want {
		t.Errorf("exact diff = %q, want %q", got, want)
	}
}

func TestDesignSlug(t *testing.T) {
	long := strings.Repeat("abcde ", 20)
	cases := []struct {
		in   string
		want string
	}{
		{"Add webhooks, with retries!", "add-webhooks-with-retries"},
		{"ADD Auth2.0 support", "add-auth2-0-support"},
		{"café menü", "caf-men"},
		{"   ", "design"},
		{"???", "design"},
		{"", "design"},
		{long, "abcde-abcde-abcde-abcde-abcde-abcde-abcd"},
	}
	for _, c := range cases {
		got := designSlug(c.in)
		if got != c.want {
			t.Errorf("designSlug(%q) = %q, want %q", c.in, got, c.want)
		}
		if len(got) > maxSlugLen {
			t.Errorf("designSlug(%q) is %d chars, cap is %d", c.in, len(got), maxSlugLen)
		}
	}
}

func TestBackupRunDir(t *testing.T) {
	at := time.Date(2026, 7, 6, 15, 30, 12, 0, time.UTC)
	want := filepath.Join("repo", ".archi", "backups", "20260706-153012")
	if got := backupRunDir("repo", at); got != want {
		t.Errorf("backupRunDir = %q, want %q", got, want)
	}
}

func TestBackupsToPrune(t *testing.T) {
	stamp := func(day int) string { return fmt.Sprintf("202601%02d-000000", day) }
	var twelve []string
	for day := 12; day >= 1; day-- { // deliberately unsorted (newest first)
		twelve = append(twelve, stamp(day))
	}
	cases := []struct {
		name  string
		names []string
		keep  int
		want  []string
	}{
		{"under cap", []string{stamp(1), stamp(2)}, 10, nil},
		{"at cap", twelve[:10], 10, nil},
		{"over cap drops oldest", twelve, 10, []string{stamp(1), stamp(2)}},
		{"keep one", []string{stamp(2), stamp(1)}, 1, []string{stamp(1)}},
	}
	for _, c := range cases {
		got := backupsToPrune(c.names, c.keep)
		if len(got) != len(c.want) {
			t.Errorf("%s: prune = %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: prune = %v, want %v", c.name, got, c.want)
			}
		}
	}
}

func TestBackupRunSaveAndPrune(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.MkdirAll(sub, 0o755)
	if err := os.WriteFile(filepath.Join(sub, "file.txt"), []byte("old content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 7, 6, 15, 30, 12, 0, time.UTC)
	run := newBackupRun(root, at)
	run.save("sub/file.txt")
	run.save("missing.txt") // absent files are skipped, not recorded
	run.finish()

	if len(run.files) != 1 || run.files[0] != "sub/file.txt" {
		t.Fatalf("run.files = %v, want [sub/file.txt]", run.files)
	}
	data, err := os.ReadFile(filepath.Join(run.dir, "sub", "file.txt"))
	if err != nil || string(data) != "old content\n" {
		t.Errorf("backup copy = %q, %v", data, err)
	}
	manifest, err := os.ReadFile(filepath.Join(run.dir, "manifest.txt"))
	if err != nil || !strings.Contains(string(manifest), "sub/file.txt") {
		t.Errorf("manifest = %q, %v", manifest, err)
	}

	// Retention: with 13 runs on disk, pruning keeps the 10 newest.
	backups := filepath.Dir(run.dir)
	for day := 1; day <= 12; day++ {
		os.MkdirAll(filepath.Join(backups, fmt.Sprintf("202606%02d-000000", day)), 0o755)
	}
	pruneBackups(backups, maxBackupRuns)
	entries, err := os.ReadDir(backups)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != maxBackupRuns {
		t.Errorf("kept %d runs, want %d", len(entries), maxBackupRuns)
	}
	if _, err := os.Stat(filepath.Join(backups, "20260601-000000")); !os.IsNotExist(err) {
		t.Error("oldest run should have been pruned")
	}
	if _, err := os.Stat(run.dir); err != nil {
		t.Error("newest run should have been kept")
	}
}

func TestDiffFingerprints(t *testing.T) {
	base := map[string]string{"a.go": "h1", "b.go": "h2", "c.go": "h3"}
	cases := []struct {
		name                     string
		cur                      map[string]string
		added, modified, removed []string
	}{
		{"identical", map[string]string{"a.go": "h1", "b.go": "h2", "c.go": "h3"}, nil, nil, nil},
		{"added", map[string]string{"a.go": "h1", "b.go": "h2", "c.go": "h3", "d.go": "h4"}, []string{"d.go"}, nil, nil},
		{"modified", map[string]string{"a.go": "h1", "b.go": "hX", "c.go": "h3"}, nil, []string{"b.go"}, nil},
		{"removed", map[string]string{"a.go": "h1", "c.go": "h3"}, nil, nil, []string{"b.go"}},
		{"mixed", map[string]string{"a.go": "hX", "c.go": "h3", "d.go": "h4", "e.go": "h5"}, []string{"d.go", "e.go"}, []string{"a.go"}, []string{"b.go"}},
		{"all replaced", map[string]string{"x.go": "hx"}, []string{"x.go"}, nil, []string{"a.go", "b.go", "c.go"}},
	}
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	for _, c := range cases {
		d := diffFingerprints(base, c.cur)
		if !eq(d.added, c.added) || !eq(d.modified, c.modified) || !eq(d.removed, c.removed) {
			t.Errorf("%s: diff = %+v, want added=%v modified=%v removed=%v", c.name, d, c.added, c.modified, c.removed)
		}
		wantEmpty := len(c.added)+len(c.modified)+len(c.removed) == 0
		if d.empty() != wantEmpty {
			t.Errorf("%s: empty() = %v, want %v", c.name, d.empty(), wantEmpty)
		}
	}
}

func TestFingerprintRoundTripDetectsStatChanges(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package a\n")
	write("sub/b.go", "package b\n")
	write("c.go", "package c\n")
	past := time.Now().Add(-time.Hour)
	for _, rel := range []string{"a.go", "sub/b.go", "c.go"} {
		if err := os.Chtimes(filepath.Join(dir, rel), past, past); err != nil {
			t.Fatal(err)
		}
	}

	files, err := scanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	fps := buildFingerprint(dir, files)
	if len(fps) != 3 {
		t.Fatalf("fingerprinted %d files, want 3: %v", len(fps), fps)
	}
	os.MkdirAll(archiDir(dir), 0o755)
	if err := writeFingerprint(dir, fps); err != nil {
		t.Fatal(err)
	}

	// The stored format is sorted "hash  path" lines.
	raw, err := os.ReadFile(fingerprintPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "  a.go") || !strings.HasSuffix(lines[1], "  c.go") || !strings.HasSuffix(lines[2], "  sub/b.go") {
		t.Errorf("fingerprint file not sorted hash/path lines: %q", raw)
	}

	loaded, err := loadFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	for rel, h := range fps {
		if loaded[rel] != h {
			t.Errorf("round trip lost %s: %q != %q", rel, loaded[rel], h)
		}
	}

	// Touch a.go (mtime only), add d.go, remove c.go — stat alone must see all three.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "a.go"), now, now); err != nil {
		t.Fatal(err)
	}
	write("d.go", "package d\n")
	os.Remove(filepath.Join(dir, "c.go"))

	files, err = scanRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := diffFingerprints(loaded, buildFingerprint(dir, files))
	if len(d.added) != 1 || d.added[0] != "d.go" {
		t.Errorf("added = %v, want [d.go]", d.added)
	}
	if len(d.modified) != 1 || d.modified[0] != "a.go" {
		t.Errorf("modified = %v, want [a.go]", d.modified)
	}
	if len(d.removed) != 1 || d.removed[0] != "c.go" {
		t.Errorf("removed = %v, want [c.go]", d.removed)
	}
	if got := d.summary(); got != "2 files added/modified, 1 removed" {
		t.Errorf("summary = %q", got)
	}
	changed := changedFiles(files, d)
	if len(changed) != 2 || changed[0].rel != "a.go" || changed[1].rel != "d.go" {
		t.Errorf("changedFiles = %v, want [a.go d.go]", changed)
	}
}

func TestNeedsFullRebuild(t *testing.T) {
	rels := func(n int) []string {
		var s []string
		for i := 0; i < n; i++ {
			s = append(s, fmt.Sprintf("f%d.go", i))
		}
		return s
	}
	cases := []struct {
		name                     string
		added, modified, removed int
		oldCount                 int
		want                     bool
	}{
		{"no previous files", 1, 0, 0, 0, true},
		{"untouched", 0, 0, 0, 50, false},
		{"small drift", 2, 1, 1, 100, false},
		{"exactly 40 percent", 20, 10, 10, 100, false},
		{"just over 40 percent", 20, 11, 10, 100, true},
		{"everything removed", 0, 0, 10, 10, true},
	}
	for _, c := range cases {
		d := fingerprintDiff{added: rels(c.added), modified: rels(c.modified), removed: rels(c.removed)}
		if got := needsFullRebuild(d, c.oldCount); got != c.want {
			t.Errorf("%s: needsFullRebuild = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFitBudget(t *testing.T) {
	// block(n) is exactly n estimated tokens of newline-terminated text
	// (n*4 bytes of 5-byte lines — use multiples of 5).
	block := func(tokens int) string { return strings.Repeat("line\n", tokens*4/5) }
	part := func(name string, tokens int, required bool) budgetPart {
		return budgetPart{name: name, required: required, content: block(tokens)}
	}
	cases := []struct {
		name         string
		parts        []budgetPart
		budget       int
		kept         []string          // output part names, in order
		actions      map[string]string // name -> truncated | dropped
		overBudgetOK bool              // a required part alone exceeds the budget
	}{
		{
			name:   "under budget untouched",
			parts:  []budgetPart{part("profile", 100, true), part("file map", 100, false), part("a.go", 100, false)},
			budget: 1000,
			kept:   []string{"profile", "file map", "a.go"},
		},
		{
			name:    "oversized map truncated to remaining room",
			parts:   []budgetPart{part("profile", 100, true), part("file map", 2500, false)},
			budget:  1000,
			kept:    []string{"profile", "file map"},
			actions: map[string]string{"file map": "truncated"},
		},
		{
			name:    "later focus file dropped once budget is spent",
			parts:   []budgetPart{part("profile", 100, true), part("file map", 100, false), part("a.go", 900, false), part("b.go", 400, false)},
			budget:  1100,
			kept:    []string{"profile", "file map", "a.go"},
			actions: map[string]string{"b.go": "dropped"},
		},
		{
			name:    "focus file truncated then rest dropped",
			parts:   []budgetPart{part("profile", 100, true), part("file map", 100, false), part("a.go", 900, false), part("b.go", 400, false)},
			budget:  700,
			kept:    []string{"profile", "file map", "a.go"},
			actions: map[string]string{"a.go": "truncated", "b.go": "dropped"},
		},
		{
			name:         "required part kept whole even over budget",
			parts:        []budgetPart{part("profile", 2000, true), part("file map", 100, false)},
			budget:       1000,
			kept:         []string{"profile"},
			actions:      map[string]string{"file map": "dropped"},
			overBudgetOK: true,
		},
	}
	for _, c := range cases {
		orig := map[string]string{}
		for _, p := range c.parts {
			orig[p.name] = p.content
		}
		got, notes := fitBudget(c.parts, c.budget)

		var names []string
		for _, p := range got {
			names = append(names, p.name)
		}
		if strings.Join(names, "|") != strings.Join(c.kept, "|") {
			t.Errorf("%s: kept %v, want %v", c.name, names, c.kept)
		}
		gotActions := map[string]string{}
		for _, n := range notes {
			gotActions[n.name] = n.action
		}
		if len(gotActions) != len(c.actions) {
			t.Errorf("%s: notes = %v, want %v", c.name, gotActions, c.actions)
		}
		for name, action := range c.actions {
			if gotActions[name] != action {
				t.Errorf("%s: note for %s = %q, want %q", c.name, name, gotActions[name], action)
			}
		}
		total := 0
		for _, p := range got {
			total += estimateTokens(p.content)
			switch c.actions[p.name] {
			case "":
				if p.content != orig[p.name] {
					t.Errorf("%s: %s should be untouched", c.name, p.name)
				}
			case "truncated":
				if !strings.HasSuffix(p.content, truncatedMark) {
					t.Errorf("%s: %s missing truncation marker", c.name, p.name)
				}
				head := strings.TrimSuffix(p.content, truncatedMark)
				if !strings.HasPrefix(orig[p.name], head) {
					t.Errorf("%s: %s truncation did not keep the head", c.name, p.name)
				}
				if len(p.content) >= len(orig[p.name]) {
					t.Errorf("%s: %s not actually smaller", c.name, p.name)
				}
			}
		}
		if !c.overBudgetOK && total > c.budget {
			t.Errorf("%s: fitted total ~%d tokens exceeds budget %d", c.name, total, c.budget)
		}
	}
}

func TestContextBudget(t *testing.T) {
	t.Setenv("ARCHI_CONTEXT_TOKENS", "")
	if got := contextBudget(); got != defaultContextTokens {
		t.Errorf("default budget = %d, want %d", got, defaultContextTokens)
	}
	t.Setenv("ARCHI_CONTEXT_TOKENS", "12000")
	if got := contextBudget(); got != 12000 {
		t.Errorf("override budget = %d, want 12000", got)
	}
	for _, bad := range []string{"banana", "-5", "0"} {
		t.Setenv("ARCHI_CONTEXT_TOKENS", bad)
		if got := contextBudget(); got != defaultContextTokens {
			t.Errorf("ARCHI_CONTEXT_TOKENS=%q: budget = %d, want default %d", bad, got, defaultContextTokens)
		}
	}
}

func TestDesignHistoryRoundTrip(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 7, 6, 15, 30, 12, 0, time.UTC)
	path := designHistoryPath(root, "Add webhooks!", at)
	if want := filepath.Join(designsDir(root), "20260706-153012-add-webhooks.md"); path != want {
		t.Fatalf("designHistoryPath = %q, want %q", path, want)
	}
	if err := writeDesignHistory(path, "Add webhooks!", "ollama/glm-5.2:cloud", at, false, "# Design"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"request: Add webhooks!", "provider: ollama/glm-5.2:cloud", "built: no", "# Design"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("history file missing %q: %q", want, data)
		}
	}

	// A second, later design lists first; the cap trims the tail.
	later := at.Add(time.Hour)
	if err := writeDesignHistory(designHistoryPath(root, "shard the events table", later), "shard the events table", "x/y", later, false, "doc"); err != nil {
		t.Fatal(err)
	}
	recs := listDesigns(root, 5)
	if len(recs) != 2 {
		t.Fatalf("listDesigns = %d records, want 2", len(recs))
	}
	if recs[0].slug != "shard-the-events-table" || recs[1].slug != "add-webhooks" {
		t.Errorf("order wrong: %+v", recs)
	}
	if recs[0].built || recs[1].built {
		t.Errorf("fresh designs should not be built: %+v", recs)
	}
	if got := listDesigns(root, 1); len(got) != 1 || got[0].slug != "shard-the-events-table" {
		t.Errorf("listDesigns cap: %+v", got)
	}

	if err := markDesignBuilt(path); err != nil {
		t.Fatal(err)
	}
	recs = listDesigns(root, 5)
	if !recs[1].built {
		t.Errorf("expected add-webhooks marked built: %+v", recs)
	}
	if recs[0].built {
		t.Errorf("other design should stay unbuilt: %+v", recs)
	}
}

// ── Phase 1: agent runtime ──────────────────────────────────────────────────

// fakeProvider scripts Complete for orchestrator tests: fixed output or error,
// optional delay honoring ctx, records prompts and the max observed concurrency.
type fakeProvider struct {
	out                   string
	err                   error
	delay                 time.Duration
	mu                    sync.Mutex
	calls                 int
	gotSystem, gotUser    []string
	inFlight, maxInFlight int
}

func (f *fakeProvider) Name() string  { return "fake/fake-1" }
func (f *fakeProvider) Model() string { return "fake-1" }

func (f *fakeProvider) Complete(ctx context.Context, system, user string, progress io.Writer) (string, error) {
	f.mu.Lock()
	f.calls++
	f.gotSystem = append(f.gotSystem, system)
	f.gotUser = append(f.gotUser, user)
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inFlight--; f.mu.Unlock() }()

	if err := sleepCtx(ctx, f.delay); err != nil {
		return "", err
	}
	if progress != nil && f.out != "" {
		mid := len(f.out) / 2
		progress.Write([]byte(f.out[:mid]))
		progress.Write([]byte(f.out[mid:]))
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return f.out, f.err
}

func traceInTempDir(t *testing.T) *Trace {
	t.Helper()
	tr, err := newTrace(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func readTraceLines(t *testing.T, dir string) []traceEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []traceEvent
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if ln == "" {
			continue
		}
		var ev traceEvent
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("bad json line %q: %v", ln, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestSpecFor(t *testing.T) {
	d, ok := specFor(RoleDesigner)
	if !ok || d.System != architectPrompt {
		t.Errorf("designer spec wrong: ok=%v system==architectPrompt=%v", ok, d.System == architectPrompt)
	}
	b, ok := specFor(RoleBuilder)
	if !ok || b.System != buildPrompt {
		t.Errorf("builder spec wrong: ok=%v", ok)
	}
	if _, ok := specFor(RoleReviewer); ok {
		t.Error("reviewer should not be registered in Phase 1")
	}
}

func TestRenderSystem(t *testing.T) {
	out, err := renderSystem(AgentSpec{Role: RoleDesigner, System: architectPrompt}, TaskInput{})
	if err != nil || out != architectPrompt {
		t.Errorf("no-action prompt must pass through byte-identically (err=%v, equal=%v)", err, out == architectPrompt)
	}
	out, err = renderSystem(AgentSpec{Role: "t", System: "hi {{.Request}}"}, TaskInput{Request: "world"})
	if err != nil || out != "hi world" {
		t.Errorf("substitution failed: %q err=%v", out, err)
	}
	if out, err := renderSystem(AgentSpec{Role: "t", System: "{{.Nope"}, TaskInput{}); err == nil || out != "" {
		t.Errorf("bad template should error with empty string, got %q err=%v", out, err)
	}
}

func TestAgentUserMessage(t *testing.T) {
	if got := agentUserMessage(TaskInput{Request: "r", Extra: map[string]string{"user_message": "VERBATIM"}}); got != "VERBATIM" {
		t.Errorf("escape hatch not verbatim: %q", got)
	}
	got := agentUserMessage(TaskInput{
		Request:   "do it",
		Interview: "Q/A",
		Grounding: []budgetPart{{name: "profile", content: "P"}, {name: "file map", content: "M"}},
	})
	for _, want := range []string{"=== PROFILE ===", "=== FILE MAP ===", "=== INTERVIEW ===", "=== REQUEST ===", "do it"} {
		if !strings.Contains(got, want) {
			t.Errorf("generic message missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "PROFILE") > strings.Index(got, "INTERVIEW") {
		t.Error("grounding must precede interview")
	}
	if strings.Index(got, "INTERVIEW") > strings.Index(got, "REQUEST") {
		t.Error("interview must precede request")
	}
}

func TestOrchestratorRun(t *testing.T) {
	fp := &fakeProvider{out: "DESIGN OUTPUT"}
	o := &Orchestrator{RunBudget: 0, Trace: nopTrace(), started: time.Now()}
	a := Agent{Spec: AgentSpec{Role: RoleDesigner, System: architectPrompt}, P: fp}
	res := o.Run(context.Background(), a, designTask("req", "USERMSG"))
	if res.Role != RoleDesigner || res.Output != "DESIGN OUTPUT" {
		t.Errorf("bad result: %+v", res)
	}
	if res.TokensIn == 0 || res.TokensOut == 0 || res.Duration < 0 {
		t.Errorf("counters unset: %+v", res)
	}
	if o.calls != 1 || o.spent != res.TokensOut || o.tokensIn != res.TokensIn {
		t.Errorf("orch counters wrong: calls=%d spent=%d in=%d", o.calls, o.spent, o.tokensIn)
	}
	if fp.gotSystem[0] != architectPrompt || fp.gotUser[0] != "USERMSG" {
		t.Errorf("prompts modified before provider: sys==prompt=%v user=%q", fp.gotSystem[0] == architectPrompt, fp.gotUser[0])
	}
}

func TestOrchestratorRunStream(t *testing.T) {
	fp := &fakeProvider{out: "streamed-output-here"}
	o := &Orchestrator{Trace: nopTrace(), started: time.Now()}
	a := Agent{Spec: AgentSpec{Role: RoleDesigner, System: "sys"}, P: fp}

	var live bytes.Buffer
	if res := o.runStream(context.Background(), a, designTask("r", "u"), &live, nil); res.Err != nil {
		t.Fatal(res.Err)
	}
	if live.String() != "streamed-output-here" {
		t.Errorf("live writer got %q", live.String())
	}

	sp := startSpinner("x") // non-TTY: SetSuffix still records the suffix
	o.runStream(context.Background(), a, designTask("r", "u"), nil, sp)
	wantSuffix := fmt.Sprintf("~%s tokens", humanCount((len(fp.out)+3)/4))
	if sp.suffix != wantSuffix {
		t.Errorf("spinner suffix = %q, want %q (must match runModel format)", sp.suffix, wantSuffix)
	}
}

func TestRunBudgetSkip(t *testing.T) {
	fp := &fakeProvider{out: strings.Repeat("x", 600)} // ~150 tokens out
	tr := traceInTempDir(t)
	o := &Orchestrator{RunBudget: 100, Trace: tr, started: time.Now()}
	a := Agent{Spec: AgentSpec{Role: RoleDesigner, System: "s"}, P: fp}

	if r1 := o.Run(context.Background(), a, designTask("r", "u")); r1.Err != nil {
		t.Fatalf("first call should succeed (overshoot allowed): %v", r1.Err)
	}
	r2 := o.Run(context.Background(), a, designTask("r", "u"))
	if !errors.Is(r2.Err, errRunBudget) {
		t.Errorf("second call should be budget-skipped, got %v", r2.Err)
	}
	if fp.calls != 1 {
		t.Errorf("provider called %d times, want 1 (second never reaches it)", fp.calls)
	}
	lines := readTraceLines(t, tr.Dir())
	if len(lines) != 2 {
		t.Fatalf("want 2 trace events, got %d", len(lines))
	}
	if lines[1].Err == "" {
		t.Error("skipped call's trace event should carry an err")
	}
}

func TestRunBudgetUnlimited(t *testing.T) {
	fp := &fakeProvider{out: strings.Repeat("y", 4000)}
	o := &Orchestrator{RunBudget: 0, Trace: nopTrace(), started: time.Now()}
	a := Agent{Spec: AgentSpec{Role: RoleDesigner, System: "s"}, P: fp}
	for i := 0; i < 5; i++ {
		if r := o.Run(context.Background(), a, designTask("r", "u")); r.Err != nil {
			t.Fatalf("call %d errored under unlimited budget: %v", i, r.Err)
		}
	}
	if fp.calls != 5 {
		t.Errorf("want 5 calls, got %d", fp.calls)
	}
}

func TestRunParallelOrder(t *testing.T) {
	o := &Orchestrator{Concurrency: 3, Trace: nopTrace(), started: time.Now()}
	var calls []AgentCall
	for i := 0; i < 5; i++ {
		fp := &fakeProvider{out: fmt.Sprintf("out-%d", i)}
		calls = append(calls, AgentCall{
			Agent: Agent{Spec: AgentSpec{Role: RoleDesigner, System: "s"}, P: fp},
			Input: designTask("r", "u"),
		})
	}
	res := o.RunParallel(context.Background(), calls)
	for i := range res {
		if res[i].Output != fmt.Sprintf("out-%d", i) {
			t.Errorf("results[%d] = %q, want out-%d", i, res[i].Output, i)
		}
	}
}

func TestRunParallelBounds(t *testing.T) {
	fp := &fakeProvider{out: "x", delay: 20 * time.Millisecond}
	o := &Orchestrator{Concurrency: 2, Trace: nopTrace(), started: time.Now()}
	var calls []AgentCall
	for i := 0; i < 6; i++ {
		calls = append(calls, AgentCall{
			Agent: Agent{Spec: AgentSpec{Role: RoleDesigner, System: "s"}, P: fp},
			Input: designTask("r", "u"),
		})
	}
	o.RunParallel(context.Background(), calls)
	fp.mu.Lock()
	m, n := fp.maxInFlight, fp.calls
	fp.mu.Unlock()
	if m > 2 {
		t.Errorf("maxInFlight = %d, want <= 2 (concurrency)", m)
	}
	if n != 6 {
		t.Errorf("calls = %d, want 6", n)
	}
}

func TestRunParallelFirstErrorCancels(t *testing.T) {
	failFp := &fakeProvider{err: errors.New("boom")}
	slowFp := &fakeProvider{out: "slow", delay: 2 * time.Second}
	o := &Orchestrator{Concurrency: 3, Trace: nopTrace(), started: time.Now()}
	calls := []AgentCall{
		{Agent: Agent{Spec: AgentSpec{Role: RoleSecCritic, System: "s"}, P: failFp}, Input: designTask("r", "u")},
		{Agent: Agent{Spec: AgentSpec{Role: RoleSimpCritic, System: "s"}, P: slowFp}, Input: designTask("r", "u")},
		{Agent: Agent{Spec: AgentSpec{Role: RoleGrndCritic, System: "s"}, P: slowFp}, Input: designTask("r", "u")},
	}
	start := time.Now()
	res := o.RunParallel(context.Background(), calls)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("siblings not cancelled promptly: took %s", elapsed)
	}
	cancelled := 0
	for _, r := range res {
		if errors.Is(r.Err, context.Canceled) {
			cancelled++
		}
	}
	if cancelled == 0 {
		t.Error("expected at least one sibling cancelled by the first hard error")
	}
}

func TestRunParallelBudgetSkipNoCancel(t *testing.T) {
	// Pre-exhaust the run budget so every call is admission-skipped. Skips must
	// return errRunBudget and must NOT cancel siblings (no context.Canceled) —
	// only a non-budget hard error is allowed to cancel (see FirstErrorCancels).
	fp := &fakeProvider{out: "unused"}
	o := &Orchestrator{Concurrency: 3, RunBudget: 100, Trace: nopTrace(), started: time.Now()}
	o.spent = 200 // already over budget before any call runs
	var calls []AgentCall
	for i := 0; i < 4; i++ {
		calls = append(calls, AgentCall{
			Agent: Agent{Spec: AgentSpec{Role: RoleDesigner, System: "s"}, P: fp},
			Input: designTask("r", "u"),
		})
	}
	res := o.RunParallel(context.Background(), calls)
	for i, r := range res {
		if !errors.Is(r.Err, errRunBudget) {
			t.Errorf("call %d: err=%v, want errRunBudget", i, r.Err)
		}
		if errors.Is(r.Err, context.Canceled) {
			t.Errorf("call %d was cancelled — a budget skip must never cancel siblings", i)
		}
	}
	if fp.calls != 0 {
		t.Errorf("provider called %d times, want 0 (all admission-skipped)", fp.calls)
	}
}

func TestNewTraceAndRecord(t *testing.T) {
	root := t.TempDir()
	tr, err := newTrace(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tr.record(AgentResult{Role: RoleDesigner, Output: "FULL DESIGN", TokensIn: 100, TokensOut: 50, Duration: time.Second}, "glm-5.2:cloud")
	tr.record(AgentResult{Role: RoleSecCritic, Output: "v", Verdict: &Verdict{Status: VerdictClean}, Err: errors.New("nope")}, "glm-5.2:cloud")

	lines := readTraceLines(t, tr.Dir())
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if lines[0].Seq != 1 || lines[1].Seq != 2 {
		t.Errorf("seqs = %d, %d", lines[0].Seq, lines[1].Seq)
	}
	if lines[0].Verdict != "" || lines[0].Err != "" {
		t.Errorf("first line should omit verdict/err, got verdict=%q err=%q", lines[0].Verdict, lines[0].Err)
	}
	if lines[1].Verdict != "clean" || lines[1].Err != "nope" {
		t.Errorf("second line verdict=%q err=%q", lines[1].Verdict, lines[1].Err)
	}
	data, err := os.ReadFile(filepath.Join(tr.Dir(), "01-designer.md"))
	if err != nil || string(data) != "FULL DESIGN" {
		t.Errorf("output file = %q err=%v", data, err)
	}
}

func TestTraceRetention(t *testing.T) {
	root := t.TempDir()
	rd := runsDir(root)
	if err := os.MkdirAll(rd, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		name := base.Add(time.Duration(i) * time.Minute).Format(stampLayout)
		if err := os.MkdirAll(filepath.Join(rd, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	later := base.Add(100 * time.Minute)
	if _, err := newTrace(root, later); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(rd)
	if len(entries) != maxRuns {
		t.Fatalf("want %d dirs, got %d", maxRuns, len(entries))
	}
	found := false
	for _, e := range entries {
		if e.Name() == later.Format(stampLayout) {
			found = true
		}
	}
	if !found {
		t.Error("newest run dir should be retained")
	}
}

func TestTraceNop(t *testing.T) {
	root := t.TempDir()
	tr := nopTrace()
	tr.record(AgentResult{Role: RoleDesigner, Output: "x"}, "m")
	if tr.Dir() != "" {
		t.Errorf("nop Dir() = %q, want empty", tr.Dir())
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("nop trace created files: %v", entries)
	}
}

func TestTraceConcurrentRecord(t *testing.T) {
	tr := traceInTempDir(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr.record(AgentResult{Role: RoleDesigner, Output: fmt.Sprintf("o%d", i)}, "m")
		}(i)
	}
	wg.Wait()
	lines := readTraceLines(t, tr.Dir())
	if len(lines) != 20 {
		t.Fatalf("want 20 lines, got %d", len(lines))
	}
	seen := map[int]bool{}
	for _, l := range lines {
		seen[l.Seq] = true
	}
	for i := 1; i <= 20; i++ {
		if !seen[i] {
			t.Errorf("missing seq %d", i)
		}
	}
}

func TestAgentConcurrencyEnv(t *testing.T) {
	t.Setenv("ARCHI_AGENT_CONCURRENCY", "")
	if agentConcurrency() != 3 {
		t.Error("empty → 3")
	}
	t.Setenv("ARCHI_AGENT_CONCURRENCY", "5")
	if agentConcurrency() != 5 {
		t.Error("5 → 5")
	}
	for _, v := range []string{"0", "-1", "x"} {
		t.Setenv("ARCHI_AGENT_CONCURRENCY", v)
		if agentConcurrency() != 3 {
			t.Errorf("%q → 3", v)
		}
	}
}

func TestRunTokensEnv(t *testing.T) {
	t.Setenv("ARCHI_RUN_TOKENS", "")
	if runTokenBudget() != 150000 {
		t.Error("empty → 150000")
	}
	t.Setenv("ARCHI_RUN_TOKENS", "9000")
	if runTokenBudget() != 9000 {
		t.Error("9000 → 9000")
	}
	t.Setenv("ARCHI_RUN_TOKENS", "junk")
	if runTokenBudget() != 150000 {
		t.Error("junk → 150000")
	}
}

func TestBoardLine(t *testing.T) {
	// useColor is false under go test (non-TTY stderr), so markers/dim are identity.
	mk := func(marker, role, state, suffix string) string {
		return strings.TrimRight("  "+marker+" "+fmt.Sprintf("%-18s", role)+" "+fmt.Sprintf("%-8s", state)+" "+suffix, " ")
	}
	cases := []struct {
		row   boardRow
		frame int
		want  string
	}{
		{boardRow{role: RoleDesigner, state: "waiting"}, 0, mk("◇", "designer", "waiting", "")},
		{boardRow{role: RoleDesigner, state: "done", suffix: "~8.2k tokens · 41s"}, 0, mk("◆", "designer", "done", "~8.2k tokens · 41s")},
		{boardRow{role: RoleSecCritic, state: "failed"}, 0, mk("✗", "sec-critic", "failed", "")},
		{boardRow{role: RoleSimpCritic, state: "running"}, 2, mk(spinnerFrames[2], "simplicity-critic", "running", "")},
	}
	for _, c := range cases {
		if got := boardLine(c.row, c.frame); got != c.want {
			t.Errorf("boardLine(%s) = %q, want %q", c.row.state, got, c.want)
		}
	}
}

func TestBoardNilSafe(t *testing.T) {
	var b *Board
	b.Set(RoleDesigner, "running", "x")
	b.Stop()
}

func TestCostLine(t *testing.T) {
	o := &Orchestrator{Trace: nopTrace(), started: time.Now()}
	if o.costLine() != "" {
		t.Error("0 calls → empty line")
	}
	o.calls, o.tokensIn, o.spent = 2, 12000, 3400
	line := o.costLine()
	if !strings.Contains(line, "2 agent calls") {
		t.Errorf("count wrong: %q", line)
	}
	if !strings.Contains(line, humanCount(12000)) || !strings.Contains(line, humanCount(3400)) {
		t.Errorf("token counts missing: %q", line)
	}
	if strings.Contains(line, "trace ") {
		t.Errorf("nop trace should add no trace path: %q", line)
	}
	o2 := &Orchestrator{Trace: traceInTempDir(t), started: time.Now()}
	o2.calls = 1
	l2 := o2.costLine()
	if !strings.Contains(l2, "1 agent call ") {
		t.Errorf("singular form wrong: %q", l2)
	}
	if !strings.Contains(l2, "trace ") {
		t.Errorf("real trace should add its path: %q", l2)
	}
}

func TestDesignPromptsUnchanged(t *testing.T) {
	fp := &fakeProvider{out: "doc"}
	o := &Orchestrator{Trace: nopTrace(), started: time.Now()}
	userMsg := designUserMessage("PROFILE", "MAP", "", "", nil, "add X")
	a := Agent{Spec: AgentSpec{Role: RoleDesigner, System: architectPrompt}, P: fp}
	o.runStream(context.Background(), a, designTask("add X", userMsg), nil, nil)
	if fp.gotSystem[0] != architectPrompt {
		t.Error("system prompt was modified on the wire")
	}
	if fp.gotUser[0] != userMsg {
		t.Errorf("user message modified on the wire:\n got %q\nwant %q", fp.gotUser[0], userMsg)
	}
}

// ── Phase 2: design ensemble ────────────────────────────────────────────────

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func count(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}

// vBlock builds a fenced verdict block.
func vBlock(status string, findings ...string) string {
	var b strings.Builder
	b.WriteString("```verdict\nstatus: " + status + "\n")
	for _, f := range findings {
		b.WriteString(f + "\n")
	}
	b.WriteString("```")
	return b.String()
}

// scriptedProvider dispatches on the identified agent role so one provider can
// script an entire ensemble run, recording call order and per-role prompts.
type scriptedProvider struct {
	mu         sync.Mutex
	fn         func(role, system, user string) (string, error)
	log        []string
	userByRole map[string]string
}

func (p *scriptedProvider) Name() string  { return "scripted/s" }
func (p *scriptedProvider) Model() string { return "s" }

func (p *scriptedProvider) Complete(ctx context.Context, system, user string, progress io.Writer) (string, error) {
	role := roleOf(system, user)
	out, err := p.fn(role, system, user)
	p.mu.Lock()
	p.log = append(p.log, role)
	if p.userByRole == nil {
		p.userByRole = map[string]string{}
	}
	p.userByRole[role] = user
	p.mu.Unlock()
	if progress != nil && out != "" {
		progress.Write([]byte(out))
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return out, err
}

func roleOf(system, user string) string {
	switch {
	case strings.Contains(system, "orienting in an unfamiliar codebase"):
		return "explorer-pick"
	case strings.Contains(system, "briefing a software architect"):
		return "explorer-brief"
	case strings.Contains(system, "security engineer reviewing"):
		return "sec"
	case strings.Contains(system, "over-engineering"):
		return "simp"
	case strings.Contains(system, "fact-checker"):
		return "grnd"
	case strings.Contains(user, "=== REVIEW FINDINGS ==="):
		return "revision"
	default:
		return "designer"
	}
}

func ensembleSession(t *testing.T, root string, cfg *Config, fn func(role, system, user string) (string, error)) (*session, *scriptedProvider) {
	t.Helper()
	sp := &scriptedProvider{fn: fn}
	mk := func(mt int) Provider { return sp }
	return &session{
		ctx: context.Background(), mk: mk, criticMk: mk, root: root, cfg: cfg,
		profile: "PROFILE", fileMap: "FILE MAP",
		opts: ensembleOpts{mode: "full", rounds: 1},
		orch: &Orchestrator{Trace: nopTrace(), started: time.Now()},
	}, sp
}

func TestParseVerdict(t *testing.T) {
	t.Run("missing_block", func(t *testing.T) {
		v, err := parseVerdict("just prose, no verdict")
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != VerdictAdvisory || len(v.Findings) != 1 || !strings.Contains(v.Findings[0].Title, "no verdict block") {
			t.Errorf("got %+v", v)
		}
	})
	t.Run("last_block_wins", func(t *testing.T) {
		s := "```verdict\nstatus: clean\n```\ntext\n```verdict\nstatus: blocking\n- [blocking] X | a.go\n```"
		v, _ := parseVerdict(s)
		if v.Status != VerdictBlocking || len(v.Findings) != 1 {
			t.Errorf("got %+v", v)
		}
	})
	t.Run("status_recomputed", func(t *testing.T) {
		v, _ := parseVerdict("```verdict\nstatus: clean\n- [blocking] X | a.go\n```")
		if v.Status != VerdictBlocking {
			t.Errorf("status should recompute to blocking, got %s", v.Status)
		}
	})
	t.Run("severity_unknown", func(t *testing.T) {
		v, _ := parseVerdict("```verdict\nstatus: advisory\n- [warning] X | a.go\n```")
		if v.Findings[0].Severity != VerdictAdvisory {
			t.Error("unknown severity should be advisory")
		}
	})
	t.Run("paths_split_trim", func(t *testing.T) {
		v, _ := parseVerdict("```verdict\nstatus: advisory\n- [advisory] X | a.go , b.go,\n- [advisory] Y\n```")
		if !equalStrings(v.Findings[0].Paths, []string{"a.go", "b.go"}) {
			t.Errorf("paths: %v", v.Findings[0].Paths)
		}
		if v.Findings[1].Paths != nil {
			t.Errorf("no pipe should give nil paths: %v", v.Findings[1].Paths)
		}
	})
	t.Run("multiline_detail_crlf", func(t *testing.T) {
		v, _ := parseVerdict("```verdict\r\nstatus: advisory\r\n- [advisory] X | a.go\r\n  line one\r\n  line two\r\n```")
		if v.Findings[0].Detail != "line one line two" {
			t.Errorf("detail: %q", v.Findings[0].Detail)
		}
	})
	t.Run("clean_empty", func(t *testing.T) {
		v, _ := parseVerdict("```verdict\nstatus: clean\n```")
		if v.Status != VerdictClean || len(v.Findings) != 0 {
			t.Errorf("got %+v", v)
		}
	})
}

func TestDowngradePathless(t *testing.T) {
	v := &Verdict{Findings: []Finding{
		{Severity: VerdictBlocking, Title: "no path"},
		{Severity: VerdictBlocking, Title: "has path", Paths: []string{"a.go"}},
	}}
	v.Status = recomputeStatus(v.Findings)
	if n := downgradePathless(v); n != 1 {
		t.Errorf("downgraded %d, want 1", n)
	}
	if v.Findings[0].Severity != VerdictAdvisory {
		t.Error("pathless blocking should become advisory")
	}
	if v.Findings[1].Severity != VerdictBlocking {
		t.Error("path-cited blocking stays blocking")
	}
	if v.Status != VerdictBlocking {
		t.Error("status should stay blocking (one blocking remains)")
	}
}

func TestParseFileRequests(t *testing.T) {
	got := parseFileRequests("prose\n```files\ninternal/a.go\n# comment\ninternal/a.go\ninternal/b.go\n\n```\ntrailing")
	if !equalStrings(got, []string{"internal/a.go", "internal/b.go"}) {
		t.Errorf("got %v", got)
	}
	var lines strings.Builder
	lines.WriteString("```files\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&lines, "f%d.go\n", i)
	}
	lines.WriteString("```")
	if n := len(parseFileRequests(lines.String())); n != maxExplorerFiles {
		t.Errorf("clamp: got %d, want %d", n, maxExplorerFiles)
	}
	if parseFileRequests("no fenced block here") != nil {
		t.Error("prose-only should return nil")
	}
}

func TestReadRequestedFiles(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "real.go"), []byte("package main\nvar Token = \"ghp_"+strings.Repeat("A", 36)+"\"\n"), 0o644)
	os.WriteFile(filepath.Join(root, "big.go"), []byte(strings.Repeat("x", maxExplorerFileBytes+1)), 0o644)
	files, dropped := readRequestedFiles(root, []string{"real.go", "missing.go", "big.go"})
	if dropped != 2 {
		t.Errorf("dropped=%d, want 2 (missing + oversized)", dropped)
	}
	if len(files) != 1 || files[0].rel != "real.go" {
		t.Fatalf("files=%v", files)
	}
	if strings.Contains(files[0].content, "ghp_") {
		t.Error("token should be redacted")
	}
	if !strings.Contains(files[0].content, "REDACTED") {
		t.Errorf("expected redaction marker in %q", files[0].content)
	}
}

func TestExplorerSkipThreshold(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644)
	run := func(tokens int) []string {
		fn := func(role, system, user string) (string, error) {
			switch role {
			case "explorer-pick":
				return "```files\nmain.go\n```", nil
			case "explorer-brief":
				return "brief", nil
			case "designer":
				return "## Summary\nD\n", nil
			default:
				return vBlock("clean"), nil
			}
		}
		sess, sp := ensembleSession(t, root, &Config{ApproxTokens: tokens}, fn)
		sess.designEnsemble("x", "", nil)
		return sp.log
	}
	if below := run(31999); count(below, "explorer-pick")+count(below, "explorer-brief") != 0 {
		t.Errorf("below threshold should not call explorer: %v", below)
	}
	if above := run(32000); count(above, "explorer-pick") != 1 || count(above, "explorer-brief") != 1 {
		t.Errorf("at threshold should call explorer twice: %v", above)
	}
}

func TestEnsembleHappyPath(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
	fn := func(role, system, user string) (string, error) {
		switch role {
		case "explorer-pick":
			return "```files\nmain.go\n```", nil
		case "explorer-brief":
			return "## Relevant modules\nmain.go | entry", nil
		case "designer":
			return "## Summary\nDraft design targeting main.go.\n", nil
		case "sec":
			return vBlock("clean"), nil
		case "simp":
			return vBlock("advisory", "- [advisory] Slightly complex | main.go"), nil
		case "grnd":
			return vBlock("blocking", "- [blocking] References missing helper | main.go"), nil
		case "revision":
			return "## Summary\nRevised design targeting main.go.\n", nil
		}
		return "", nil
	}
	sess, sp := ensembleSession(t, root, &Config{ApproxTokens: 40000}, fn)
	doc, err := sess.designEnsemble("add caching", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.log) != 7 {
		t.Fatalf("want 7 calls, got %d: %v", len(sp.log), sp.log)
	}
	if sp.log[0] != "explorer-pick" || sp.log[1] != "explorer-brief" || sp.log[2] != "designer" {
		t.Errorf("prefix wrong: %v", sp.log)
	}
	critics := map[string]bool{sp.log[3]: true, sp.log[4]: true, sp.log[5]: true}
	for _, r := range []string{"sec", "simp", "grnd"} {
		if !critics[r] {
			t.Errorf("missing critic %s in %v", r, sp.log)
		}
	}
	if sp.log[6] != "revision" {
		t.Errorf("last call should be revision: %v", sp.log)
	}
	if !strings.Contains(doc, "Revised design") {
		t.Error("final doc missing revised text")
	}
	if !strings.Contains(doc, "## Review notes") {
		t.Error("final doc missing review notes")
	}
	if !strings.Contains(doc, "blocking, addressed in revision") {
		t.Errorf("notes missing resolved-blocking marker:\n%s", doc)
	}
}

func TestEnsembleRounds2(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644)
	fn := func(role, system, user string) (string, error) {
		switch role {
		case "designer":
			return "## Summary\nDraft.\n", nil
		case "revision":
			return "## Summary\nRevised.\n", nil
		case "grnd":
			return vBlock("blocking", "- [blocking] X | main.go"), nil
		default:
			return vBlock("clean"), nil
		}
	}
	sess, sp := ensembleSession(t, root, &Config{ApproxTokens: 0}, fn)
	sess.opts.rounds = 2
	doc, err := sess.designEnsemble("add X", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := count(sp.log, "grnd"); c != 2 {
		t.Errorf("want 2 critic passes, got %d: %v", c, sp.log)
	}
	if c := count(sp.log, "revision"); c != 2 {
		t.Errorf("want 2 revisions, got %d", c)
	}
	if !strings.Contains(doc, "round 2 of 2") {
		t.Errorf("notes should report round 2 of 2:\n%s", doc)
	}
}

func TestEnsembleCriticFails(t *testing.T) {
	root := t.TempDir()
	fn := func(role, system, user string) (string, error) {
		switch role {
		case "designer":
			return "## Summary\nDraft.\n", nil
		case "sec":
			return vBlock("clean"), nil
		case "simp":
			return vBlock("advisory", "- [advisory] Y | main.go"), nil
		case "grnd":
			return "", errors.New("critic boom")
		}
		return "", nil
	}
	sess, _ := ensembleSession(t, root, &Config{ApproxTokens: 0}, fn)
	doc, err := sess.designEnsemble("add X", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "grounding critic unavailable") {
		t.Errorf("notes missing unavailable line:\n%s", doc)
	}
	if !strings.Contains(doc, "simplicity · advisory") {
		t.Errorf("surviving critic should still render:\n%s", doc)
	}
}

func TestEnsembleBudgetMidPanel(t *testing.T) {
	root := t.TempDir()
	fn := func(role, system, user string) (string, error) {
		if role == "designer" {
			return "## Summary\n" + strings.Repeat("x", 4000) + "\n", nil // ~1000 tokens
		}
		return vBlock("clean"), nil
	}
	sess, _ := ensembleSession(t, root, &Config{ApproxTokens: 0}, fn)
	sess.orch.RunBudget = 100 // the designer alone exhausts it
	doc, err := sess.designEnsemble("add X", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "unavailable") {
		t.Errorf("budget-skipped critics should render unavailable:\n%s", doc)
	}
}

func TestEnsembleExplorerFallback(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644)
	fn := func(role, system, user string) (string, error) {
		switch role {
		case "explorer-pick":
			return "no files here, just prose", nil // garbage → no requests
		case "designer":
			return "## Summary\nDraft.\n", nil
		default:
			return vBlock("clean"), nil
		}
	}
	sess, sp := ensembleSession(t, root, &Config{ApproxTokens: 40000}, fn)
	if _, err := sess.designEnsemble("add X", "", nil); err != nil {
		t.Fatal(err)
	}
	if count(sp.log, "explorer-brief") != 0 {
		t.Errorf("brief step should be skipped on fallback: %v", sp.log)
	}
	if strings.Contains(sp.userByRole["designer"], "GROUNDING BRIEF") {
		t.Error("designer must get classic grounding (no brief) on explorer fallback")
	}
}

func TestRenderReviewNotes(t *testing.T) {
	clean := []AgentResult{
		{Role: RoleSecCritic, Verdict: &Verdict{Status: VerdictClean}},
		{Role: RoleSimpCritic, Verdict: &Verdict{Status: VerdictClean}},
		{Role: RoleGrndCritic, Verdict: &Verdict{Status: VerdictClean}},
	}
	got := renderReviewNotes(clean, 1, 1, false)
	if !strings.Contains(got, "No findings — the panel signed off on the first draft.") {
		t.Errorf("clean case:\n%s", got)
	}
	if !strings.Contains(got, "round 1 of 1") {
		t.Error("header missing round")
	}

	mixed := []AgentResult{
		{Role: RoleSecCritic, Verdict: &Verdict{Status: VerdictAdvisory, Findings: []Finding{{Severity: VerdictAdvisory, Title: "Log hygiene", Detail: "Avoid logging tokens", Paths: []string{"a.go"}}}}},
		{Role: RoleSimpCritic, Verdict: &Verdict{Status: VerdictBlocking, Findings: []Finding{{Severity: VerdictBlocking, Title: "Too many layers", Paths: []string{"b.go", "c.go"}}}}},
		{Role: RoleGrndCritic, Err: errRunBudget},
	}
	got = renderReviewNotes(mixed, 2, 2, true)
	for _, want := range []string{
		"- **security · advisory** — Log hygiene",
		"  Avoid logging tokens _(a.go)_",
		"- **simplicity · blocking, addressed in revision** — Too many layers",
		"  _(b.go, c.go)_",
		"- _grounding critic unavailable (skipped: run budget exhausted)_",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mixed missing %q in:\n%s", want, got)
		}
	}
	if got := renderReviewNotes(mixed, 1, 1, false); !strings.Contains(got, "blocking, unresolved") {
		t.Errorf("unresolved variant:\n%s", got)
	}
}

func TestFastModeGolden(t *testing.T) {
	withBrief := designUserMessage("P", "M", "the brief", "", nil, "R")
	noBrief := designUserMessage("P", "M", "", "", nil, "R")
	if strings.Contains(noBrief, "GROUNDING BRIEF") {
		t.Error("empty brief must add nothing (fast mode byte-identical)")
	}
	if !strings.Contains(withBrief, "GROUNDING BRIEF (from the explorer)") {
		t.Error("non-empty brief must be inserted")
	}
	if spec, _ := specFor(RoleDesigner); spec.System != architectPrompt {
		t.Error("designer system prompt must stay architectPrompt")
	}
}

func TestEnsembleOptsPrecedence(t *testing.T) {
	if o := resolveEnsembleOpts(&Config{}, "", "", 0); o.mode != "full" || o.rounds != 1 || o.criticModel != "" {
		t.Errorf("defaults: %+v", o)
	}
	if o := resolveEnsembleOpts(&Config{Agents: "fast", Rounds: 2, CriticModel: "cheap"}, "", "", 0); o.mode != "fast" || o.rounds != 2 || o.criticModel != "cheap" {
		t.Errorf("config: %+v", o)
	}
	if o := resolveEnsembleOpts(&Config{Agents: "fast", Rounds: 2}, "full", "x", 3); o.mode != "full" || o.rounds != 3 || o.criticModel != "x" {
		t.Errorf("flag override: %+v", o)
	}
	if o := resolveEnsembleOpts(&Config{}, "", "", 9); o.rounds != 3 {
		t.Errorf("rounds clamp: %d", o.rounds)
	}

	root := t.TempDir()
	os.MkdirAll(archiDir(root), 0o755)
	cfg := &Config{}
	persistEnsembleFlags(cfg, root, true, false, true, "off", "", 2)
	loaded, err := loadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agents != "off" || loaded.Rounds != 2 {
		t.Errorf("persisted: %+v", loaded)
	}
	if loaded.CriticModel != "" {
		t.Error("critic-model not passed → should not persist")
	}

	// A save failure warns and does not panic.
	root2 := t.TempDir()
	os.WriteFile(archiDir(root2), []byte("not a dir"), 0o644)
	persistEnsembleFlags(&Config{}, root2, true, false, false, "fast", "", 0)
}

func TestRevisionUserMessage(t *testing.T) {
	got := revisionUserMessage("THE DESIGN", "THE VERDICTS")
	for _, want := range []string{"=== CURRENT DESIGN ===", "THE DESIGN", "=== REVIEW FINDINGS ===", "THE VERDICTS", "resolve every blocking finding", "Trade-off:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestGroundingCriticEval(t *testing.T) {
	if os.Getenv("ARCHI_EVAL") != "1" {
		t.Skip("set ARCHI_EVAL=1 with provider creds to run the grounding-critic eval")
	}
	t.Skip("eval harness requires a live provider; not run in unit tests")
}

// ── Phase 3: build swarm, verify, rollback ──────────────────────────────────

func contains(xs []string, x string) bool { return count(xs, x) > 0 }

func fileBlock(path, content string) string {
	return "=== FILE: " + path + " ===\n```go\n" + content + "\n```\n"
}

func packetIDFromUser(user string) string {
	for _, line := range strings.Split(user, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "id: ") {
			return strings.TrimSpace(strings.TrimPrefix(t, "id: "))
		}
	}
	return ""
}

// swarmProvider scripts builder responses by packet id.
type swarmProvider struct {
	mu       sync.Mutex
	respond  func(id string) (string, error)
	log      []string
	userByID map[string]string
}

func (p *swarmProvider) Name() string  { return "sw/s" }
func (p *swarmProvider) Model() string { return "s" }
func (p *swarmProvider) Complete(ctx context.Context, system, user string, progress io.Writer) (string, error) {
	id := packetIDFromUser(user)
	p.mu.Lock()
	p.log = append(p.log, id)
	if p.userByID == nil {
		p.userByID = map[string]string{}
	}
	p.userByID[id] = user
	p.mu.Unlock()
	out, err := p.respond(id)
	if progress != nil && out != "" {
		progress.Write([]byte(out))
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return out, err
}

func swarmSession(root string, p Provider) *session {
	mk := func(mt int) Provider { return p }
	return &session{
		ctx: context.Background(), mk: mk, criticMk: mk, root: root,
		profile: "P", fileMap: "M",
		orch: &Orchestrator{Trace: nopTrace(), started: time.Now()},
	}
}

func TestTopoLevels(t *testing.T) {
	chain := []packet{{ID: "a", Files: []string{"a"}}, {ID: "b", Files: []string{"b"}, DependsOn: []string{"a"}}, {ID: "c", Files: []string{"c"}, DependsOn: []string{"b"}}}
	levels, err := topoLevels(chain)
	if err != nil || len(levels) != 3 {
		t.Fatalf("chain: %v %v", levels, err)
	}
	diamond := []packet{
		{ID: "a", Files: []string{"a"}},
		{ID: "b", Files: []string{"b"}, DependsOn: []string{"a"}},
		{ID: "c", Files: []string{"c"}, DependsOn: []string{"a"}},
		{ID: "d", Files: []string{"d"}, DependsOn: []string{"b", "c"}},
	}
	levels, err = topoLevels(diamond)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(levels[0], []string{"a"}) || !equalStrings(levels[1], []string{"b", "c"}) || !equalStrings(levels[2], []string{"d"}) {
		t.Errorf("diamond levels: %v", levels)
	}
	cyc := []packet{{ID: "a", Files: []string{"a"}, DependsOn: []string{"b"}}, {ID: "b", Files: []string{"b"}, DependsOn: []string{"a"}}}
	if _, err := topoLevels(cyc); err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "a") {
		t.Errorf("cycle should error naming members: %v", err)
	}
	self := []packet{{ID: "a", Files: []string{"a"}, DependsOn: []string{"a"}}}
	if _, err := topoLevels(self); err == nil {
		t.Error("self-dependency should be a cycle")
	}
}

func TestParsePackets(t *testing.T) {
	good := "```packets\n- id: types\n  title: Types\n  files: a.go\n  dependsOn:\n  notes: only types\n- id: store\n  title: Store\n  files: b.go, b_test.go\n  dependsOn: types\n```"
	ps, err := parsePackets(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].ID != "types" || !equalStrings(ps[1].Files, []string{"b.go", "b_test.go"}) || !equalStrings(ps[1].DependsOn, []string{"types"}) {
		t.Errorf("parsed wrong: %+v", ps)
	}
	if _, err := parsePackets("no block"); err == nil {
		t.Error("missing block should error")
	}
	if _, err := parsePackets("```packets\n- id:\n  files: a.go\n```"); err == nil {
		t.Error("empty id should error")
	}
	if _, err := parsePackets("```packets\n- id: x\n  title: X\n```"); err == nil {
		t.Error("empty files should error")
	}
	if _, err := parsePackets("```packets\n- id: x\n  files: a.go\n- id: x\n  files: b.go\n```"); err == nil {
		t.Error("duplicate id should error")
	}
}

func TestValidatePackets(t *testing.T) {
	if err := validatePackets([]packet{{ID: "a", Files: []string{"a.go"}, DependsOn: []string{"missing"}}}); err == nil {
		t.Error("unknown dep should error")
	}
	if err := validatePackets([]packet{{ID: "a", Files: []string{"x.go"}}, {ID: "b", Files: []string{"x.go"}}}); err == nil {
		t.Error("shared file should error")
	}
	if err := validatePackets([]packet{{ID: "a", Files: []string{"../escape.go"}}}); err == nil {
		t.Error("unsafe path should error")
	}
	var nine []packet
	for i := 0; i < 9; i++ {
		nine = append(nine, packet{ID: fmt.Sprintf("p%d", i), Files: []string{fmt.Sprintf("f%d.go", i)}})
	}
	if err := validatePackets(nine); err == nil {
		t.Error("9 packets should exceed maxPackets")
	}
}

func TestShouldSwarm(t *testing.T) {
	big := "## Changes by area\n| Area/Path | Change |\n|---|---|\n| a.go | new |\n| b.go | new |\n| c/d.go | new |\n| e.go | mod |\n"
	if !shouldSwarm(big) {
		t.Errorf("4 paths should swarm: count=%d", countDesignPaths(big))
	}
	small := "## Changes by area\n| Area/Path | Change |\n|---|---|\n| a.go | new |\n"
	if shouldSwarm(small) {
		t.Errorf("too few paths should not swarm: count=%d", countDesignPaths(small))
	}
	if shouldSwarm("## Summary\nno table here") {
		t.Error("missing section should not swarm")
	}
}

func TestRunSwarmScheduling(t *testing.T) {
	root := t.TempDir()
	ps := []packet{
		{ID: "types", Files: []string{"types.go"}},
		{ID: "store", Files: []string{"store.go"}, DependsOn: []string{"types"}},
		{ID: "http", Files: []string{"http.go"}, DependsOn: []string{"types"}},
	}
	sp := &swarmProvider{respond: func(id string) (string, error) {
		switch id {
		case "types":
			return fileBlock("types.go", "package main\ntype T int"), nil
		case "store":
			return fileBlock("store.go", "package main\nfunc S(){}"), nil
		case "http":
			return fileBlock("http.go", "package main\nfunc H(){}"), nil
		}
		return "", nil
	}}
	s := swarmSession(root, sp)
	merged, _, err := runSwarm(context.Background(), s.orch, s, "design", ps)
	if err != nil {
		t.Fatal(err)
	}
	if sp.log[0] != "types" {
		t.Errorf("types must build first: %v", sp.log)
	}
	if !contains(sp.log[1:], "store") || !contains(sp.log[1:], "http") {
		t.Errorf("store+http must build in wave 2: %v", sp.log)
	}
	if len(merged) != 3 {
		t.Errorf("want 3 merged files, got %d", len(merged))
	}
	if !strings.Contains(sp.userByID["store"], "STUBS FROM COMPLETED PACKETS") || !strings.Contains(sp.userByID["store"], "packet types") {
		t.Errorf("store should receive types' stubs:\n%s", sp.userByID["store"])
	}
}

func TestRunSwarmFailureSkips(t *testing.T) {
	root := t.TempDir()
	ps := []packet{
		{ID: "types", Files: []string{"types.go"}},
		{ID: "store", Files: []string{"store.go"}, DependsOn: []string{"types"}},
		{ID: "wire", Files: []string{"wire.go"}, DependsOn: []string{"store"}},
	}
	sp := &swarmProvider{respond: func(id string) (string, error) {
		switch id {
		case "types":
			return fileBlock("types.go", "package main\ntype T int"), nil
		case "store":
			return "", errors.New("store boom")
		case "wire":
			return fileBlock("wire.go", "package main"), nil
		}
		return "", nil
	}}
	s := swarmSession(root, sp)
	merged, results, err := runSwarm(context.Background(), s.orch, s, "design", ps)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]packetResult{}
	for _, r := range results {
		byID[r.pkt.ID] = r
	}
	if byID["store"].err == nil {
		t.Error("store should be failed")
	}
	if !byID["wire"].skipped {
		t.Errorf("wire should be skipped (dep store failed): %+v", byID["wire"])
	}
	if byID["types"].err != nil {
		t.Error("types should still succeed")
	}
	if len(merged) != 1 {
		t.Errorf("only types should merge, got %d files", len(merged))
	}
}

func TestRunSwarmCtxCancel(t *testing.T) {
	root := t.TempDir()
	ps := []packet{{ID: "a", Files: []string{"a.go"}}}
	sp := &swarmProvider{respond: func(id string) (string, error) { return fileBlock("a.go", "x"), nil }}
	s := swarmSession(root, sp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := runSwarm(ctx, s.orch, s, "d", ps); err == nil {
		t.Error("cancelled ctx should return an error and apply nothing")
	}
}

func TestBuilderOutputFiltered(t *testing.T) {
	pkt := packet{ID: "x", Files: []string{"a.go"}}
	out := filterToPacket(pkt, []fileChange{{path: "a.go", content: "ok"}, {path: "b.go", content: "nope"}})
	if len(out) != 1 || out[0].path != "a.go" {
		t.Errorf("out-of-scope file should be dropped: %+v", out)
	}
}

func TestMergeDisjoint(t *testing.T) {
	base := "l1\nl2\nl3\nl4\nl5\n"
	a := "TOP\nl2\nl3\nl4\nl5\n"    // edits line 1
	b := "l1\nl2\nl3\nl4\nBOTTOM\n" // edits line 5
	m, ok := mergeDisjoint(base, a, b)
	if !ok || !strings.Contains(m, "TOP") || !strings.Contains(m, "BOTTOM") {
		t.Errorf("disjoint edits should union: ok=%v m=%q", ok, m)
	}
	if _, ok := mergeDisjoint(base, "l1\nXX\nl3\nl4\nl5\n", "l1\nYY\nl3\nl4\nl5\n"); ok {
		t.Error("edits to the same line should not be disjoint")
	}
	if _, ok := mergeDisjoint(base, "l1\nl2\nAA\nl3\nl4\nl5\n", "l1\nl2\nBB\nl3\nl4\nl5\n"); ok {
		t.Error("insertions at the same index should not be disjoint")
	}
	big := strings.Repeat("x\n", maxDiffLines+1)
	if _, ok := mergeDisjoint(big, big, big); ok {
		t.Error("oversized input should decline")
	}
}

func TestMergeOutputsDependencyWins(t *testing.T) {
	root := t.TempDir()
	ps := []packet{{ID: "b", Files: []string{"x.go"}}, {ID: "a", Files: []string{"y.go"}, DependsOn: []string{"b"}}}
	results := []packetResult{
		{pkt: ps[0], changes: []fileChange{{path: "shared.go", content: "B version"}}},
		{pkt: ps[1], changes: []fileChange{{path: "shared.go", content: "A version"}}},
	}
	arbCalled := false
	arb := func(path, base, a, b string) (string, error) { arbCalled = true; return "", nil }
	merged, err := mergeOutputs(root, results, ps, arb)
	if err != nil {
		t.Fatal(err)
	}
	if arbCalled {
		t.Error("arbitration must not run when dependency order decides")
	}
	if len(merged) != 1 || merged[0].content != "A version" {
		t.Errorf("dependent a's version should win: %+v", merged)
	}
}

func TestMergeOutputsArbitration(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "shared.go"), []byte("line1\nline2\nline3\n"), 0o644)
	ps := []packet{{ID: "a", Files: []string{"a.go"}}, {ID: "b", Files: []string{"b.go"}}}
	results := []packetResult{
		{pkt: ps[0], changes: []fileChange{{path: "shared.go", content: "line1\nAAA\nline3\n"}}},
		{pkt: ps[1], changes: []fileChange{{path: "shared.go", content: "line1\nBBB\nline3\n"}}},
	}
	arbCalls := 0
	var gotBase, gotA, gotB string
	arb := func(path, base, a, b string) (string, error) {
		arbCalls++
		gotBase, gotA, gotB = base, a, b
		return "MERGED", nil
	}
	merged, err := mergeOutputs(root, results, ps, arb)
	if err != nil {
		t.Fatal(err)
	}
	if arbCalls != 1 {
		t.Errorf("arbitration should run exactly once, ran %d", arbCalls)
	}
	if !strings.Contains(gotBase, "line2") || !strings.Contains(gotA, "AAA") || !strings.Contains(gotB, "BBB") {
		t.Error("arbitration should receive base + both versions")
	}
	if merged[0].content != "MERGED" {
		t.Errorf("arbitration output should be used: %q", merged[0].content)
	}
}

func TestDetectToolchain(t *testing.T) {
	mk := func(files map[string]string) string {
		d := t.TempDir()
		for name, body := range files {
			os.WriteFile(filepath.Join(d, name), []byte(body), 0o644)
		}
		return d
	}
	if tc, ok := detectToolchain(mk(map[string]string{"go.mod": "module x"})); !ok || tc.name != "go" || len(tc.cmds) != 2 {
		t.Errorf("go.mod → go build+vet, got %+v ok=%v", tc, ok)
	}
	if tc, ok := detectToolchain(mk(map[string]string{"package.json": `{"scripts":{"test":"jest"}}`})); !ok || tc.name != "npm" {
		t.Errorf("real npm test → npm, got %+v ok=%v", tc, ok)
	}
	if _, ok := detectToolchain(mk(map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`})); ok {
		t.Error("npm default test should not match")
	}
	if tc, ok := detectToolchain(mk(map[string]string{"Cargo.toml": "[package]"})); !ok || tc.name != "cargo" {
		t.Errorf("Cargo.toml → cargo, got %+v", tc)
	}
	if tc, ok := detectToolchain(mk(map[string]string{"Makefile": "build:\n\tgo build\ntest:\n\tgo test\n"})); !ok || tc.name != "make" {
		t.Errorf("Makefile test target → make, got %+v", tc)
	}
	if _, ok := detectToolchain(mk(map[string]string{})); ok {
		t.Error("empty dir → no toolchain")
	}
	if tc, _ := detectToolchain(mk(map[string]string{"go.mod": "module x", "Makefile": "test:\n\tx\n"})); tc.name != "go" {
		t.Error("go should win over make")
	}
}

func TestTruncateOutput(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	out := truncateOutput(b.String(), 80, 40)
	if !strings.Contains(out, "line0") || !strings.Contains(out, "line299") {
		t.Error("should keep first and last lines")
	}
	if !strings.Contains(out, "omitted") {
		t.Error("should note omitted lines")
	}
	if got := truncateOutput("a\nb\nc", 80, 40); got != "a\nb\nc" {
		t.Errorf("short text unchanged: %q", got)
	}
}

func TestMapErrorsToFiles(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.go", "b.go"} {
		os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644)
	}
	out := "a.go:41:15: undefined: X\nb.go:10: oops\na.go:9: again\nmissing.go:1: nope\n/etc/passwd:1: bad\n../x.go:2: bad\n"
	files := mapErrorsToFiles(out, root)
	if !equalStrings(files, []string{"a.go", "b.go"}) {
		t.Errorf("expected [a.go b.go] deduped, got %v", files)
	}
}

func TestManifestV2RoundTrip(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "over.go"), []byte("orig"), 0o644)
	run := newBackupRun(root, time.Now())
	run.save("over.go")
	run.markCreated("new.go")
	run.finish()
	data, err := os.ReadFile(filepath.Join(run.dir, "manifest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "#v2\n") {
		t.Errorf("manifest should start with #v2: %q", data)
	}
	restore, created := parseManifest(data)
	if !equalStrings(restore, []string{"over.go"}) || !equalStrings(created, []string{"new.go"}) {
		t.Errorf("parsed: restore=%v created=%v", restore, created)
	}
	// v1 compatibility
	r1, c1 := parseManifest([]byte("old/a.go\nold/b.go\n"))
	if !equalStrings(r1, []string{"old/a.go", "old/b.go"}) || len(c1) != 0 {
		t.Errorf("v1 parse: %v %v", r1, c1)
	}
}

func TestApplyChangesRecordsCreated(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "exist.go"), []byte("old\n"), 0o644)
	changes := []fileChange{
		{path: "exist.go", content: "new content\n"},
		{path: "fresh.go", content: "brand new\n"},
	}
	applied, stamp, err := applyChanges(root, changes, true)
	if err != nil || !applied || stamp == "" {
		t.Fatalf("applied=%v stamp=%q err=%v", applied, stamp, err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".archi", "backups", stamp, "manifest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	restore, created := parseManifest(data)
	if !equalStrings(restore, []string{"exist.go"}) {
		t.Errorf("overwritten should be B: %v", restore)
	}
	if !equalStrings(created, []string{"fresh.go"}) {
		t.Errorf("new should be C: %v", created)
	}
}

func TestRollbackRoundTrip(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "keep.go"), []byte("ORIGINAL\n"), 0o644)
	os.WriteFile(filepath.Join(root, "gone.go"), []byte("DELETE ME\n"), 0o644)
	changes := []fileChange{
		{path: "keep.go", content: "MODIFIED\n"},
		{path: "made.go", content: "CREATED\n"},
		{path: "gone.go", del: true},
	}
	_, stamp, err := applyChanges(root, changes, true)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, ".archi", "backups", stamp)
	data, _ := os.ReadFile(filepath.Join(runDir, "manifest.txt"))
	restore, created := parseManifest(data)
	if err := restoreRun(root, runDir, restore, created); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "keep.go")); string(b) != "ORIGINAL\n" {
		t.Errorf("overwritten file not restored: %q", b)
	}
	if _, err := os.Stat(filepath.Join(root, "made.go")); !os.IsNotExist(err) {
		t.Error("created file should be deleted on rollback")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "gone.go")); string(b) != "DELETE ME\n" {
		t.Errorf("deleted file not restored: %q", b)
	}
}

func TestListBackupRuns(t *testing.T) {
	dir := t.TempDir()
	for _, stamp := range []string{"20260101-000000", "20260102-000000"} {
		os.MkdirAll(filepath.Join(dir, stamp), 0o755)
		os.WriteFile(filepath.Join(dir, stamp, "manifest.txt"), []byte("#v2\nB\ta.go\nC\tb.go\n"), 0o644)
	}
	os.MkdirAll(filepath.Join(dir, "20260103-000000"), 0o755)
	os.WriteFile(filepath.Join(dir, "20260103-000000", "manifest.txt"), []byte("legacy.go\n"), 0o644)
	runs := listBackupRuns(dir)
	if len(runs) != 3 {
		t.Fatalf("want 3 runs, got %d", len(runs))
	}
	if runs[0].stamp != "20260103-000000" {
		t.Errorf("newest first: %v", runs[0].stamp)
	}
	if runs[0].restored != 1 || runs[0].created != 0 {
		t.Errorf("v1 counts: %+v", runs[0])
	}
	if runs[1].restored != 1 || runs[1].created != 1 {
		t.Errorf("v2 counts: %+v", runs[1])
	}
}

// ── Phase 4+5: reviewers & memory ───────────────────────────────────────────

func TestSplitDiffByFile(t *testing.T) {
	multi := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/y.go b/y.go\n+++ b/y.go\n+z\n"
	files := splitDiffByFile(multi)
	if len(files) != 2 || files[0].path != "x.go" || files[1].path != "y.go" {
		t.Errorf("multi-file: %+v", files)
	}
	bf := splitDiffByFile("--- a/z.go\n+++ b/z.go\n@@ -1 +1 @@\n-a\n+b\n")
	if len(bf) != 1 || bf[0].path != "z.go" {
		t.Errorf("bare patch: %+v", bf)
	}
	if splitDiffByFile("") != nil {
		t.Error("empty diff should be nil")
	}
}

func TestPackReviewChunks(t *testing.T) {
	if len(packReviewChunks([]diffFile{{path: "a", body: "x"}, {path: "b", body: "y"}}, 1000)) != 1 {
		t.Error("small files should fit in one chunk")
	}
	f := diffFile{path: "a", body: strings.Repeat("x", 400)}
	g := diffFile{path: "b", body: strings.Repeat("y", 400)}
	if n := len(packReviewChunks([]diffFile{f, g}, 120)); n != 2 {
		t.Errorf("should split at boundary: %d", n)
	}
	ch := packReviewChunks([]diffFile{{path: "big", body: strings.Repeat("z", 10000)}}, 100)
	if len(ch) != 1 || estimateTokens(ch[0][0].body) > 130 {
		t.Errorf("oversized file should be truncated: %d tokens", estimateTokens(ch[0][0].body))
	}
}

func TestReviewExitCode(t *testing.T) {
	if reviewExitCode([]*Verdict{{Status: VerdictClean}}) != 0 {
		t.Error("clean → 0")
	}
	if reviewExitCode([]*Verdict{{Status: VerdictAdvisory}}) != 0 {
		t.Error("advisory → 0")
	}
	if reviewExitCode([]*Verdict{{Status: VerdictClean}, {Status: VerdictBlocking}}) != 1 {
		t.Error("any blocking → 1")
	}
	if reviewExitCode([]*Verdict{nil, {Status: VerdictClean}}) != 0 {
		t.Error("nil verdicts ignored")
	}
}

func TestRenderReviewDoc(t *testing.T) {
	results := []reviewResult{
		{Role: RoleSecCritic, Verdict: &Verdict{Status: VerdictBlocking, Findings: []Finding{{Severity: VerdictBlocking, Title: "SQLi", Detail: "use params", Paths: []string{"a.go"}}}}},
		{Role: RoleSimpCritic, Verdict: &Verdict{Status: VerdictAdvisory, Findings: []Finding{{Severity: VerdictAdvisory, Title: "dup", Detail: "reuse", Paths: []string{"b.go"}}}}},
		{Role: RoleGrndCritic, Skipped: "no .archi cache"},
	}
	doc := renderReviewDoc("git diff HEAD", results)
	for _, want := range []string{"# archi review — git diff HEAD", "| security | **blocking** | 1 |", "_skipped — no .archi cache_", "## Blocking", "1. **SQLi** — `a.go`", "## Advisory", "exit 1"} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %q in:\n%s", want, doc)
		}
	}
	clean := renderReviewDoc("x", []reviewResult{{Role: RoleSecCritic, Verdict: &Verdict{Status: VerdictClean}}})
	if strings.Contains(clean, "## Blocking") || strings.Contains(clean, "## Advisory") {
		t.Error("clean review should omit finding sections")
	}
	if !strings.Contains(clean, "exit 0") {
		t.Error("clean review exit 0")
	}
}

func TestReadDesignDoc(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(designsDir(root), 0o755)
	path := designHistoryPath(root, "add X", time.Now())
	if err := writeDesignHistory(path, "add X", "prov", time.Now(), true, "## Summary\nbody\n"); err != nil {
		t.Fatal(err)
	}
	fm, body, err := readDesignDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if fm["request"] != "add X" || fm["built"] != "yes" {
		t.Errorf("front-matter: %v", fm)
	}
	if !strings.Contains(body, "## Summary") {
		t.Errorf("body: %q", body)
	}
}

func TestResolveDesignArg(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(designsDir(root), 0o755)
	if _, err := resolveDesignArg(root, "latest"); err == nil {
		t.Error("no designs → error")
	}
	writeDesignHistory(designHistoryPath(root, "draft one", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), "draft one", "p", time.Now(), false, "d")
	if p, err := resolveDesignArg(root, "latest"); err != nil || !strings.Contains(p, "draft-one") {
		t.Errorf("draft fallback: %v %v", p, err)
	}
	writeDesignHistory(designHistoryPath(root, "built two", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)), "built two", "p", time.Now(), true, "d")
	if p, _ := resolveDesignArg(root, "latest"); !strings.Contains(p, "built-two") {
		t.Errorf("should pick newest built: %v", p)
	}
}

func TestExtractRepoPaths(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "real.go"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, "internal"), 0o755)
	os.WriteFile(filepath.Join(root, "internal", "svc.go"), []byte("x"), 0o644)
	paths := extractRepoPaths(root, "Touches `real.go` and internal/svc.go and missing.go and real.go again.", 20)
	if !contains(paths, "real.go") || !contains(paths, "internal/svc.go") {
		t.Errorf("expected real paths: %v", paths)
	}
	if contains(paths, "missing.go") {
		t.Error("nonexistent path should be excluded")
	}
	if count(paths, "real.go") != 1 {
		t.Error("paths should dedupe")
	}
}

func TestSplitPlanAndFiles(t *testing.T) {
	plan, blocks := splitPlanAndFiles("## Test plan\ntable\n=== FILE: a_test.go ===\n```go\ncode\n```\n")
	if !strings.Contains(plan, "Test plan") || strings.Contains(plan, "=== FILE") {
		t.Errorf("plan: %q", plan)
	}
	if !strings.Contains(blocks, "=== FILE: a_test.go ===") {
		t.Errorf("blocks: %q", blocks)
	}
	if p, b := splitPlanAndFiles("just a plan, no files"); p != "just a plan, no files" || b != "" {
		t.Errorf("plan only: %q %q", p, b)
	}
}

func TestDesignDocsSince(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(designsDir(root), 0o755)
	writeDesignHistory(designHistoryPath(root, "old one", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), "old one", "p", time.Now(), true, "old body")
	writeDesignHistory(designHistoryPath(root, "new one", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), "new one", "p", time.Now(), true, "new body")
	if all := designDocsSince(root, ""); len(all) != 2 {
		t.Errorf("all: %d", len(all))
	}
	recent := designDocsSince(root, "20260201-000000")
	if len(recent) != 1 || !strings.Contains(recent[0].content, "new body") {
		t.Errorf("since filter: %+v", recent)
	}
}

func TestSplitMemoryEntries(t *testing.T) {
	if len(splitMemoryEntries("")) != 0 {
		t.Error("empty → 0 entries")
	}
	e := splitMemoryEntries("preamble\n## 2026-01-01 a\nbody a\n## 2026-01-02 b\nbody b\n")
	if len(e) != 2 || e[0].heading != "## 2026-01-01 a" || e[1].body != "body b" {
		t.Errorf("entries: %+v", e)
	}
}

func TestFitMemory(t *testing.T) {
	s := "## 2026-01-01 a\n" + strings.Repeat("x ", 100) + "\n## 2026-01-02 b\nshort\n"
	out := fitMemory(s, 20)
	if !strings.Contains(out, "newest first") || !strings.Contains(out, "2026-01-02 b") {
		t.Errorf("newest-first: %q", out)
	}
	if strings.Contains(out, "2026-01-01 a") {
		t.Error("older entry should be omitted under a tight budget")
	}
	if !strings.Contains(out, "older entries omitted") {
		t.Error("omission trailer expected")
	}
	all := fitMemory(s, 100000)
	if !strings.Contains(all, "2026-01-01 a") || !strings.Contains(all, "2026-01-02 b") {
		t.Error("generous budget should include all")
	}
	if fitMemory("", 100) != "" {
		t.Error("empty → empty")
	}
}

func TestFactLines(t *testing.T) {
	m := factLines("- fact one\ntext\n- fact two\n**Decided:** not a bullet")
	if !m["- fact one"] || !m["- fact two"] || m["**Decided:** not a bullet"] {
		t.Errorf("factLines: %v", m)
	}
}

func TestDedupEntryFacts(t *testing.T) {
	out := dedupEntryFacts("**Facts:**\n- Deploys on Fly.io\n- New unique fact", map[string]bool{"- Deploys on Fly.io": true})
	if strings.Contains(out, "Deploys on Fly.io") {
		t.Error("duplicate fact should be removed")
	}
	if !strings.Contains(out, "New unique fact") || !strings.Contains(out, "**Facts:**") {
		t.Errorf("unique/non-bullet lines kept: %q", out)
	}
}

func TestAppendMemoryEntry(t *testing.T) {
	root := t.TempDir()
	tm := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if err := appendMemoryEntry(root, "add webhooks", tm, "**Decided:** X\n**Facts:**\n- Fact A"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(memoryPath(root))
	if !strings.Contains(string(data), "## 2026-07-06 add-webhooks") {
		t.Errorf("heading format: %q", data)
	}
	appendMemoryEntry(root, "another", tm, "**Decided:** Y\n**Facts:**\n- Fact A\n- Fact B")
	data, _ = os.ReadFile(memoryPath(root))
	if strings.Count(string(data), "- Fact A") != 1 {
		t.Error("Fact A should not be duplicated")
	}
	if !strings.Contains(string(data), "- Fact B") {
		t.Error("Fact B should be added")
	}
}

func TestMemoryStats(t *testing.T) {
	root := t.TempDir()
	if n, _ := memoryStats(root); n != 0 {
		t.Error("no memory → 0")
	}
	os.MkdirAll(memoryDir(root), 0o755)
	os.WriteFile(memoryPath(root), []byte("## 2026-01-01 a\nx\n## 2026-07-04 b\ny\n"), 0o644)
	if n, last := memoryStats(root); n != 2 || last != "2026-07-04" {
		t.Errorf("stats: %d %q", n, last)
	}
}

func TestValidCompactedMemory(t *testing.T) {
	orig := "## a\nx\n## b\ny\n## c\nz\n## d\nw\n## e\nv\n"
	if validCompactedMemory(orig, "") {
		t.Error("empty is invalid")
	}
	if validCompactedMemory(orig, "no headings") {
		t.Error("heading-less is invalid")
	}
	if validCompactedMemory(orig, "## only\none") {
		t.Error("1 of 5 (<25%) is invalid")
	}
	if !validCompactedMemory(orig, "## a\nx\n## b\ny\n") {
		t.Error("2 of 5 is valid")
	}
}

func TestDocStamp(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(archiDir(root), 0o755)
	if lastDocStamp(root) != "" {
		t.Error("missing → empty")
	}
	if err := writeDocStamp(root, time.Date(2026, 7, 6, 15, 30, 12, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := lastDocStamp(root); got != "20260706-153012" {
		t.Errorf("round-trip: %q", got)
	}
}

func TestMemoryInDesignMessage(t *testing.T) {
	withMem := designUserMessage("P", "M", "", "MEMORY-TEXT", nil, "R")
	if !strings.Contains(withMem, "TEAM MEMORY") || !strings.Contains(withMem, "MEMORY-TEXT") {
		t.Error("non-empty memory should insert a section")
	}
	if strings.Contains(designUserMessage("P", "M", "", "", nil, "R"), "TEAM MEMORY") {
		t.Error("empty memory must add nothing")
	}
}
