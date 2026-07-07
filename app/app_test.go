package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── helpers ─────────────────────────────────────────────────────────────────

func loadTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	pemBytes, err := os.ReadFile(filepath.Join("testdata", "test_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := loadPrivateKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func needGit(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("git fixture tests are POSIX-only")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func runCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// mkBareRepo seeds <gitBase>/<repo>.git with files on main and returns the
// path of a working clone the test can keep committing from.
func mkBareRepo(t *testing.T, gitBase, repo string, files map[string]string) string {
	t.Helper()
	bare := filepath.Join(gitBase, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "", "git", "init", "--bare", "-b", "main", bare)
	seed := t.TempDir()
	runCmd(t, "", "git", "clone", bare, seed)
	for rel, content := range files {
		p := filepath.Join(seed, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitC(t, seed, "add", "-A")
	gitC(t, seed, "commit", "-m", "seed")
	gitC(t, seed, "push", "origin", "HEAD:main")
	return seed
}

// gitC runs git with a throwaway identity (commits need one).
func gitC(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)
	return runCmd(t, dir, "git", full...)
}

// writeStubArchi writes an executable archi stand-in that records its argv
// to $ARCHI_STUB_LOG, copies any -f file to $ARCHI_STUB_LOG.req, and prints
// canned output per subcommand.
func writeStubArchi(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archi")
	script := `#!/bin/sh
echo "$@" >> "$ARCHI_STUB_LOG"
prev=""
for a in "$@"; do
  if [ "$prev" = "-f" ]; then cp "$a" "$ARCHI_STUB_LOG.req"; fi
  prev="$a"
done
case "$1" in
  version) echo "archi 9.9.9" ;;
  init)
    mkdir -p .archi/backups .archi/designs
    echo "{}" > .archi/config.json
    echo "profile" > .archi/profile.md
    echo "b" > .archi/backups/b.txt
    echo "d" > .archi/designs/d.md
    ;;
  design)
    if [ -n "$ARCHI_STUB_SLEEP" ]; then exec sleep "$ARCHI_STUB_SLEEP"; fi
    echo "generated code" > generated.go
    echo "# Design: stub"
    echo ""
    echo "The plan."
    ;;
  review)
    [ -f pr.diff ] || { echo "missing pr.diff" >&2; exit 2; }
    printf '# archi review — patch\n\n---\n_2 findings · 1 blocking · exit 1_\n'
    exit 1
    ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeGitHub is an httptest GitHub API recording what the app does to it.
type fakeGitHub struct {
	mu        sync.Mutex
	mints     int
	perm      map[string]string // user → permission
	archiYML  string            // "" ⇒ 404
	diff      string
	prJSON    map[int]string // PR number → JSON body
	comments  []string
	reactions []int64
	reviews   []string
	checks    []map[string]any
	createdPR []map[string]any
	srv       *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	fg := &fakeGitHub{perm: map[string]string{}, prJSON: map[int]string{}}
	fg.srv = httptest.NewServer(fg.handler())
	t.Cleanup(fg.srv.Close)
	return fg
}

var (
	reactionsRe = regexp.MustCompile(`^/repos/.+/issues/comments/(\d+)/reactions$`)
	commentsRe  = regexp.MustCompile(`^/repos/.+/issues/(\d+)/comments$`)
	pullRe      = regexp.MustCompile(`^/repos/.+/pulls/(\d+)$`)
)

func (fg *fakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fg.mu.Lock()
		defer fg.mu.Unlock()
		p := r.URL.Path
		writeJSON := func(code int, v any) {
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.Method == "POST" && strings.HasPrefix(p, "/app/installations/"):
			fg.mints++
			writeJSON(201, map[string]string{
				"token":      "t0k",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		case r.Method == "GET" && strings.HasSuffix(p, "/permission"):
			parts := strings.Split(p, "/")
			user := parts[len(parts)-2]
			perm, ok := fg.perm[user]
			if !ok {
				writeJSON(404, map[string]string{"message": "Not Found"})
				return
			}
			writeJSON(200, map[string]string{"permission": perm})
		case r.Method == "POST" && reactionsRe.MatchString(p):
			var id int64
			fmt.Sscanf(reactionsRe.FindStringSubmatch(p)[1], "%d", &id)
			fg.reactions = append(fg.reactions, id)
			writeJSON(201, map[string]any{})
		case r.Method == "POST" && commentsRe.MatchString(p):
			var body struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			fg.comments = append(fg.comments, body.Body)
			writeJSON(201, map[string]any{"id": 99})
		case r.Method == "GET" && strings.Contains(p, "/contents/"):
			if fg.archiYML == "" {
				writeJSON(404, map[string]string{"message": "Not Found"})
				return
			}
			writeJSON(200, map[string]string{
				"content": base64.StdEncoding.EncodeToString([]byte(fg.archiYML)),
			})
		case r.Method == "GET" && pullRe.MatchString(p):
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				w.WriteHeader(200)
				fmt.Fprint(w, fg.diff)
				return
			}
			var num int
			fmt.Sscanf(pullRe.FindStringSubmatch(p)[1], "%d", &num)
			body, ok := fg.prJSON[num]
			if !ok {
				writeJSON(404, map[string]string{"message": "Not Found"})
				return
			}
			w.WriteHeader(200)
			fmt.Fprint(w, body)
		case r.Method == "POST" && strings.HasSuffix(p, "/pulls"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			fg.createdPR = append(fg.createdPR, body)
			writeJSON(201, map[string]any{"number": 43, "html_url": "https://github.com/o/r/pull/43"})
		case r.Method == "POST" && strings.HasSuffix(p, "/reviews"):
			var body struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			fg.reviews = append(fg.reviews, body.Body)
			writeJSON(200, map[string]any{})
		case r.Method == "POST" && strings.HasSuffix(p, "/check-runs"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			fg.checks = append(fg.checks, body)
			writeJSON(201, map[string]any{})
		default:
			writeJSON(404, map[string]string{"message": "no fake route for " + r.Method + " " + p})
		}
	})
}

// testEnv builds an envConfig for pipeline tests.
func testEnv(t *testing.T, archiBin, gitBase string) envConfig {
	t.Helper()
	return envConfig{
		AppID: "7", WebhookSecret: "s3cret",
		ArchiBin: archiBin, CacheDir: t.TempDir(), WorkDir: t.TempDir(),
		Workers: 1, QueueSize: 8, MaxJobsPerHour: 10,
		JobTimeout: 30 * time.Second,
		gitBase:    gitBase,
	}
}

// newTestServer wires a server against the fake API.
func newTestServer(t *testing.T, fg *fakeGitHub, env envConfig) *server {
	t.Helper()
	s := &server{
		env: env,
		auth: &appAuth{
			appID: env.AppID, key: loadTestKey(t), base: fg.srv.URL,
			http: fg.srv.Client(), now: time.Now, cache: map[int64]instToken{},
		},
		now:     time.Now,
		apiBase: fg.srv.URL,
		httpc:   fg.srv.Client(),
	}
	s.queue = newJobQueue(env.QueueSize, s.runJob)
	return s
}

// postWebhook signs and delivers one webhook to the handler.
func postWebhook(t *testing.T, h http.Handler, secret, event string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Delivery", "d-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func drainJob(t *testing.T, q *jobQueue) (job, bool) {
	t.Helper()
	select {
	case j := <-q.ch:
		return j, true
	default:
		return job{}, false
	}
}

// ── webhook signature ───────────────────────────────────────────────────────

func TestVerifySignature(t *testing.T) {
	// GitHub's published example vector.
	secret := []byte("It's a Secret to Everybody")
	body := []byte("Hello, World!")
	const good = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if !verifySignature(secret, body, good) {
		t.Error("published vector must verify")
	}
	cases := []struct {
		name, header string
		secret       []byte
	}{
		{"wrong secret", good, []byte("nope")},
		{"missing header", "", secret},
		{"bad prefix", "sha1=757107ea", secret},
		{"odd-length hex", "sha256=abc", secret},
		{"tampered", "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e18", secret},
	}
	for _, c := range cases {
		if verifySignature(c.secret, body, c.header) {
			t.Errorf("%s: must not verify", c.name)
		}
	}
}

// ── app auth ────────────────────────────────────────────────────────────────

func TestAppJWTGolden(t *testing.T) {
	key := loadTestKey(t)
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	a := &appAuth{appID: "12345", key: key, now: func() time.Time { return now }}
	tok, err := a.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 segments, got %d", len(parts))
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(header) != `{"alg":"RS256","typ":"JWT"}` {
		t.Errorf("header = %s (%v)", header, err)
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	want := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"12345"}`, now.Add(-60*time.Second).Unix(), now.Add(540*time.Second).Unix())
	if err != nil || string(claims) != want {
		t.Errorf("claims = %s, want %s (%v)", claims, want, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
}

func TestLoadPrivateKey(t *testing.T) {
	pkcs1, err := os.ReadFile(filepath.Join("testdata", "test_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadPrivateKey(pkcs1); err != nil {
		t.Errorf("PKCS#1: %v", err)
	}

	key := loadTestKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := loadPrivateKey(pkcs8); err != nil {
		t.Errorf("PKCS#8 RSA: %v", err)
	}

	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})
	if _, err := loadPrivateKey(ecPEM); err == nil {
		t.Error("PKCS#8 EC key must be rejected")
	}
	if _, err := loadPrivateKey([]byte("garbage")); err == nil {
		t.Error("garbage must be rejected")
	}
}

func TestInstallationToken(t *testing.T) {
	start := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	now := start
	mints := 0
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(401)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/app/installations/9/access_tokens" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Errorf("missing Bearer JWT, got %q", r.Header.Get("Authorization"))
		}
		mints++
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{
			"token":      fmt.Sprintf("tok-%d", mints),
			"expires_at": start.Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	a := &appAuth{appID: "1", key: loadTestKey(t), base: srv.URL, http: srv.Client(),
		now: func() time.Time { return now }, cache: map[int64]instToken{}}
	ctx := context.Background()

	tok, err := a.installationToken(ctx, 9)
	if err != nil || tok != "tok-1" {
		t.Fatalf("first mint: %q, %v", tok, err)
	}
	if tok, _ := a.installationToken(ctx, 9); tok != "tok-1" || mints != 1 {
		t.Errorf("second call should be cached: %q, mints=%d", tok, mints)
	}
	now = start.Add(56 * time.Minute) // <5m validity left ⇒ re-mint
	if tok, _ := a.installationToken(ctx, 9); tok != "tok-2" || mints != 2 {
		t.Errorf("expiring token should re-mint: %q, mints=%d", tok, mints)
	}
	fail = true
	now = start.Add(2 * time.Hour)
	if _, err := a.installationToken(ctx, 9); err == nil {
		t.Error("API 401 must surface")
	}
}

// ── command parsing ─────────────────────────────────────────────────────────

func TestParseCommand(t *testing.T) {
	cases := []struct {
		name, body string
		want       *command
		ok         bool
	}{
		{"design one-line", "/archi design add webhooks", &command{Name: "design", Request: "add webhooks"}, true},
		{"design multi-line", "/archi design add webhooks\nwith retries and HMAC", &command{Name: "design", Request: "add webhooks\nwith retries and HMAC"}, true},
		{"quoted request unwrapped", `/archi design "add webhooks"`, &command{Name: "design", Request: "add webhooks"}, true},
		{"backtick unwrapped", "/archi design `add webhooks`", &command{Name: "design", Request: "add webhooks"}, true},
		{"pr flag after design", "/archi design --pr add webhooks", &command{Name: "design", PR: true, Request: "add webhooks"}, true},
		{"pr flag before design", "/archi --pr design add webhooks", &command{Name: "design", PR: true, Request: "add webhooks"}, true},
		{"bare archi", "/archi", &command{Name: "help"}, true},
		{"unknown sub", "/archi frobnicate", &command{Name: "help"}, true},
		{"build", "/archi build", &command{Name: "build"}, true},
		{"review", "/archi review", &command{Name: "review"}, true},
		{"leading blank lines", "\n\n  /archi review", &command{Name: "review"}, true},
		{"not our comment", "great work, ship it", nil, false},
		{"archive is not archi", "/archive this thread", nil, false},
		{"empty design request", "/archi design", &command{Name: "help"}, true},
		{"empty design request with pr", "/archi design --pr", &command{Name: "help", PR: true}, true},
	}
	for _, c := range cases {
		got, ok := parseCommand(c.body)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Name != c.want.Name || got.PR != c.want.PR || got.Request != c.want.Request {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}

	if got, ok := parseCommand("/archi design " + strings.Repeat("x", maxRequestBytes)); !ok || got.Name != "help" || !strings.Contains(got.helpNote, "too large") {
		t.Errorf(">64 KiB must be refused with help: %+v", got)
	}
	if got, ok := parseCommand("/archi design bad\x00byte"); !ok || got.Name != "help" || !strings.Contains(got.helpNote, "NUL") {
		t.Errorf("NUL must be refused with help: %+v", got)
	}
	if got, _ := parseCommand("/archi design"); !strings.Contains(got.helpNote, "design needs a request") {
		t.Errorf("empty design request note missing: %+v", got)
	}
}

// ── repo config ─────────────────────────────────────────────────────────────

func TestParseRepoConfig(t *testing.T) {
	full := `# archi config
provider: anthropic        # ollama | anthropic | openai
model: claude-fable-5
critic_model: claude-haiku-4-5

agents: full
auto_review: true
max_tokens: 12000
`
	rc, warns := parseRepoConfig([]byte(full))
	want := repoConfig{Provider: "anthropic", Model: "claude-fable-5", CriticModel: "claude-haiku-4-5",
		Agents: "full", AutoReview: true, MaxTokens: 12000}
	if rc != want {
		t.Errorf("full example: got %+v, want %+v", rc, want)
	}
	if len(warns) != 0 {
		t.Errorf("full example warns: %v", warns)
	}

	cases := []struct {
		name, line, wantWarn string
	}{
		{"unknown key", "colour: blue", "unknown key"},
		{"bad bool", "auto_review: yes", "auto_review"},
		{"bad int", "max_tokens: many", "max_tokens"},
		{"bad agents", "agents: turbo", "agents"},
		{"nested block", "jobs:", "full YAML is not supported"},
		{"list item", "- provider: x", "full YAML is not supported"},
		{"indented", "  model: x", "full YAML is not supported"},
	}
	for _, c := range cases {
		_, warns := parseRepoConfig([]byte(c.line))
		if len(warns) != 1 || !strings.Contains(warns[0], c.wantWarn) {
			t.Errorf("%s: warns = %v, want one containing %q", c.name, warns, c.wantWarn)
		}
	}

	if rc, warns := parseRepoConfig(nil); rc != (repoConfig{}) || len(warns) != 0 {
		t.Errorf("empty file must be pure defaults, got %+v %v", rc, warns)
	}
}

// ── comment splitting ───────────────────────────────────────────────────────

func TestSplitComment(t *testing.T) {
	if parts := splitComment("short", 100); len(parts) != 1 || parts[0] != "short" {
		t.Errorf("under limit: %v", parts)
	}

	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "line %02d with some padding text\n", i)
	}
	parts := splitComment(b.String(), 300)
	if len(parts) < 2 {
		t.Fatalf("expected a split, got %d part(s)", len(parts))
	}
	for i, p := range parts {
		if len(p) > 300 {
			t.Errorf("part %d is %d bytes, over the max", i, len(p))
		}
	}
	if !strings.Contains(parts[1], fmt.Sprintf("_…continued (part 2/%d)_", len(parts))) {
		t.Errorf("part 2 header missing: %q", parts[1][:60])
	}
	joined := strings.Join(parts, "\n")
	if !strings.Contains(joined, "line 00") || !strings.Contains(joined, "line 39") {
		t.Error("content lost in split")
	}

	// A fenced block spanning the split is closed and re-opened.
	var f strings.Builder
	f.WriteString("intro\n```go\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&f, "code line %02d ................\n", i)
	}
	f.WriteString("```\ntail")
	fparts := splitComment(f.String(), 300)
	if len(fparts) < 2 {
		t.Fatalf("expected a split, got %d", len(fparts))
	}
	if !strings.HasSuffix(strings.TrimSpace(fparts[0]), "```") {
		t.Errorf("part 1 must close the fence:\n%s", fparts[0])
	}
	afterHeader := strings.SplitN(fparts[1], "\n\n", 2)
	if len(afterHeader) != 2 || !strings.HasPrefix(afterHeader[1], "```go\n") {
		t.Errorf("part 2 must re-open the fence:\n%s", fparts[1])
	}
}

// ── review mapping ──────────────────────────────────────────────────────────

func TestParseReviewFooter(t *testing.T) {
	doc := "# archi review — stdin\n\n| Critic | Verdict | Findings |\n\n---\n_3 findings · 1 blocking · exit 1_\n"
	o := parseReviewFooter(doc, 1)
	if o.Findings != 3 || o.Blocking != 1 || o.Exit != 1 {
		t.Errorf("footer parse: %+v", o)
	}
	o = parseReviewFooter("no footer here", 2)
	if o.Findings != 0 || o.Blocking != 0 || o.Exit != 2 {
		t.Errorf("absent footer must keep process exit: %+v", o)
	}
}

func TestConclusionFor(t *testing.T) {
	cases := []struct {
		name     string
		o        reviewOutcome
		ranOK    bool
		wantConc string
	}{
		{"clean", reviewOutcome{Exit: 0}, true, "success"},
		{"advisory", reviewOutcome{Exit: 0, Findings: 2}, true, "neutral"},
		{"blocking", reviewOutcome{Exit: 1, Findings: 3, Blocking: 1}, true, "failure"},
		{"operational exit", reviewOutcome{Exit: 2}, true, "neutral"},
		{"did not run", reviewOutcome{}, false, "neutral"},
	}
	for _, c := range cases {
		conc, title := conclusionFor(c.o, c.ranOK)
		if conc != c.wantConc {
			t.Errorf("%s: conclusion = %s, want %s", c.name, conc, c.wantConc)
		}
		if title == "" {
			t.Errorf("%s: empty title", c.name)
		}
	}
	if _, title := conclusionFor(reviewOutcome{}, false); title != "archi review could not run" {
		t.Errorf("could-not-run title = %q", title)
	}
}

func TestTruncateCheckText(t *testing.T) {
	if got := truncateCheckText("short"); got != "short" {
		t.Errorf("under limit changed: %q", got)
	}
	long := strings.Repeat("a line of filler text\n", 5000)
	got := truncateCheckText(long)
	if len(got) > checkTextMax {
		t.Errorf("still over limit: %d", len(got))
	}
	if !strings.HasSuffix(got, "_…truncated — full review posted as a PR comment._") {
		t.Error("trailer missing")
	}
	if strings.Contains(got, "filler text\n\n\n") {
		t.Error("unexpected mangling")
	}
}

// ── rate gate ───────────────────────────────────────────────────────────────

func TestRepoGateAllow(t *testing.T) {
	g := &repoGate{}
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if !g.allow(now, 3) {
			t.Fatalf("start %d should be allowed", i)
		}
	}
	if g.allow(now.Add(time.Minute), 3) {
		t.Error("4th start within the hour must be refused")
	}
	if !g.allow(now.Add(61*time.Minute), 3) {
		t.Error("window must slide: a start an hour later is allowed")
	}
}

// ── child env & redaction ───────────────────────────────────────────────────

func TestArchiEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-x",
		"OLLAMA_HOST=http://localhost:11434",
		"ARCHI_CONTEXT_TOKENS=9000",
		"ARCHI_TIMEOUT=99h",
		"HOME=/root",
		"AWS_SECRET_ACCESS_KEY=leak",
		"LANG=C",
	}
	got := archiEnv(base, 15*time.Minute)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=sk-x", "OLLAMA_HOST=http://localhost:11434", "ARCHI_CONTEXT_TOKENS=9000", "ARCHI_TIMEOUT=15m0s"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in child env", want)
		}
	}
	for _, drop := range []string{"AWS_SECRET_ACCESS_KEY", "LANG=", "HOME=", "ARCHI_TIMEOUT=99h"} {
		if strings.Contains(joined, drop) {
			t.Errorf("%q must be dropped from child env", drop)
		}
	}
}

func TestRedactGit(t *testing.T) {
	out := "fatal: unable to access 'https://x-access-token:ghs_abc123@github.com/o/r.git': ghs_abc123 again ghs_abc123"
	got := redactGit(out, "ghs_abc123")
	if strings.Contains(got, "ghs_abc123") {
		t.Errorf("token leaked: %q", got)
	}
	if !strings.Contains(got, "***@github.com") {
		t.Errorf("userinfo not scrubbed: %q", got)
	}
	// Unknown token still scrubbed via the userinfo pattern.
	got = redactGit("push to https://x-access-token:OTHER@github.com/x.git failed", "")
	if strings.Contains(got, "OTHER") {
		t.Errorf("userinfo token leaked with empty known token: %q", got)
	}
	if redactGit("plain output", "") != "plain output" {
		t.Error("empty token must be a no-op on plain text")
	}
}

// ── ghClient request shapes ─────────────────────────────────────────────────

func TestGHClient(t *testing.T) {
	type recorded struct {
		method, path, accept, auth, version string
		body                                map[string]any
	}
	var last recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = recorded{method: r.Method, path: r.URL.Path, accept: r.Header.Get("Accept"),
			auth: r.Header.Get("Authorization"), version: r.Header.Get("X-GitHub-Api-Version")}
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&last.body)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/fail"):
			w.WriteHeader(422)
			json.NewEncoder(w).Encode(map[string]string{"message": "Validation Failed"})
		case strings.Contains(r.Header.Get("Accept"), "diff"):
			fmt.Fprint(w, "diff --git a/x b/x\n")
		case strings.HasSuffix(r.URL.Path, "/permission"):
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
		case strings.Contains(r.URL.Path, "/contents/"):
			json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte("auto_review: true\n")) + "\n"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "number": 12, "html_url": "u", "permission": "write"})
		}
	}))
	defer srv.Close()
	c := &ghClient{base: srv.URL, token: "tok", http: srv.Client()}
	ctx := context.Background()

	id, err := c.postComment(ctx, "o/r", 4, "hello")
	if err != nil || id != 7 {
		t.Errorf("postComment: id=%d err=%v", id, err)
	}
	if last.method != "POST" || last.path != "/repos/o/r/issues/4/comments" || last.body["body"] != "hello" {
		t.Errorf("postComment shape: %+v", last)
	}
	if last.auth != "Bearer tok" || last.version != "2022-11-28" || !strings.Contains(last.accept, "vnd.github+json") {
		t.Errorf("headers: %+v", last)
	}

	if err := c.react(ctx, "o/r", 55); err != nil {
		t.Errorf("react: %v", err)
	}
	if last.path != "/repos/o/r/issues/comments/55/reactions" || last.body["content"] != "eyes" {
		t.Errorf("react shape: %+v", last)
	}

	if perm, err := c.permission(ctx, "o/r", "ghost"); err != nil || perm != "none" {
		t.Errorf("permission 404 must read as none: %q %v", perm, err)
	}

	data, err := c.fileAt(ctx, "o/r", "main", ".github/archi.yml")
	if err != nil || string(data) != "auto_review: true\n" {
		t.Errorf("fileAt: %q %v", data, err)
	}

	diff, err := c.prDiff(ctx, "o/r", 12)
	if err != nil || !strings.HasPrefix(diff, "diff --git") {
		t.Errorf("prDiff: %q %v", diff, err)
	}
	if !strings.Contains(last.accept, "application/vnd.github.v3.diff") {
		t.Errorf("prDiff Accept: %q", last.accept)
	}

	num, url, err := c.createPR(ctx, "o/r", "t", "h", "b", "body", true)
	if err != nil || num != 12 || url != "u" {
		t.Errorf("createPR: %d %q %v", num, url, err)
	}
	if last.body["draft"] != true || last.body["head"] != "h" {
		t.Errorf("createPR shape: %+v", last.body)
	}

	if err := c.createReview(ctx, "o/r", 12, "rev"); err != nil {
		t.Errorf("createReview: %v", err)
	}
	if last.body["event"] != "COMMENT" {
		t.Errorf("createReview shape: %+v", last.body)
	}

	if err := c.createCheckRun(ctx, "o/r", checkRun{Name: checkName, HeadSHA: "sha", Status: "completed", Conclusion: "success"}); err != nil {
		t.Errorf("createCheckRun: %v", err)
	}
	if last.body["name"] != checkName || last.body["head_sha"] != "sha" {
		t.Errorf("createCheckRun shape: %+v", last.body)
	}

	err = c.do(ctx, "POST", "/repos/o/r/fail", map[string]string{}, nil)
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("non-2xx error text: %v", err)
	}
}

// ── webhook → job ───────────────────────────────────────────────────────────

func issueCommentPayload(user, userType, body string, issue int, onPR bool) []byte {
	m := map[string]any{
		"action": "created",
		"comment": map[string]any{
			"id":   501,
			"body": body,
			"user": map[string]any{"login": user, "type": userType},
		},
		"issue":        map[string]any{"number": issue},
		"repository":   map[string]any{"full_name": "o/r", "default_branch": "main"},
		"installation": map[string]any{"id": 1},
	}
	if onPR {
		m["issue"].(map[string]any)["pull_request"] = map[string]any{"url": "x"}
	}
	data, _ := json.Marshal(m)
	return data
}

func TestWebhookToJob(t *testing.T) {
	fg := newFakeGitHub(t)
	fg.perm["alice"] = "write"
	fg.perm["bob"] = "read"
	fg.prJSON[7] = `{"head":{"ref":"feature","sha":"abc123","repo":{"full_name":"o/r"}},"base":{"ref":"main","repo":{"full_name":"o/r"}}}`
	env := testEnv(t, "archi", t.TempDir())
	s := newTestServer(t, fg, env)
	h := s.handler()

	// Happy path: design command enqueues and reacts.
	w := postWebhook(t, h, env.WebhookSecret, "issue_comment", issueCommentPayload("alice", "User", "/archi design add webhooks", 42, false))
	if w.Code != 202 {
		t.Fatalf("design command: status %d", w.Code)
	}
	j, ok := drainJob(t, s.queue)
	if !ok || j.Kind != jobDesign || j.Request != "add webhooks" || j.User != "alice" || j.Repo != "o/r" || j.Issue != 42 {
		t.Errorf("enqueued job = %+v", j)
	}
	if len(fg.reactions) != 1 || fg.reactions[0] != 501 {
		t.Errorf("eyes reaction missing: %v", fg.reactions)
	}

	// Bot comments are ignored outright.
	w = postWebhook(t, h, env.WebhookSecret, "issue_comment", issueCommentPayload("archi[bot]", "Bot", "/archi design x", 42, false))
	if w.Code != 200 {
		t.Errorf("bot comment: status %d", w.Code)
	}
	if _, ok := drainJob(t, s.queue); ok {
		t.Error("bot comment must not enqueue")
	}

	// Read-only user is refused with a comment, before any job.
	nc := len(fg.comments)
	w = postWebhook(t, h, env.WebhookSecret, "issue_comment", issueCommentPayload("bob", "User", "/archi design x", 42, false))
	if w.Code != 200 {
		t.Errorf("read-only: status %d", w.Code)
	}
	if _, ok := drainJob(t, s.queue); ok {
		t.Error("read-only user must not enqueue")
	}
	if len(fg.comments) != nc+1 || !strings.Contains(fg.comments[nc], "write access") {
		t.Errorf("refusal comment: %v", fg.comments[nc:])
	}

	// /archi review on a PR pulls head/base from the API.
	w = postWebhook(t, h, env.WebhookSecret, "issue_comment", issueCommentPayload("alice", "User", "/archi review", 7, true))
	if w.Code != 202 {
		t.Fatalf("review command: status %d", w.Code)
	}
	j, _ = drainJob(t, s.queue)
	if j.Kind != jobReview || j.PRNumber != 7 || j.HeadSHA != "abc123" || j.BaseRef != "main" {
		t.Errorf("review job = %+v", j)
	}

	// /archi build on a non-design branch is refused.
	nc = len(fg.comments)
	w = postWebhook(t, h, env.WebhookSecret, "issue_comment", issueCommentPayload("alice", "User", "/archi build", 7, true))
	if _, ok := drainJob(t, s.queue); ok {
		t.Error("build on non-design branch must not enqueue")
	}
	if len(fg.comments) != nc+1 || !strings.Contains(fg.comments[nc], "isn't an archi design PR") {
		t.Errorf("build refusal: %v", fg.comments[nc:])
	}

	// Bare /archi gets usage help.
	nc = len(fg.comments)
	postWebhook(t, h, env.WebhookSecret, "issue_comment", issueCommentPayload("alice", "User", "/archi", 42, false))
	if len(fg.comments) != nc+1 || !strings.Contains(fg.comments[nc], "archi commands") {
		t.Errorf("help reply: %v", fg.comments[nc:])
	}

	// auto_review: true enqueues on pull_request opened.
	fg.archiYML = "auto_review: true\n"
	prPayload, _ := json.Marshal(map[string]any{
		"action": "opened", "number": 9,
		"pull_request": map[string]any{
			"head": map[string]any{"ref": "f", "sha": "s9", "repo": map[string]any{"full_name": "fork/r"}},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "o/r"}},
		},
		"repository":   map[string]any{"full_name": "o/r", "default_branch": "main"},
		"installation": map[string]any{"id": 1},
	})
	w = postWebhook(t, h, env.WebhookSecret, "pull_request", prPayload)
	if w.Code != 202 {
		t.Fatalf("auto-review: status %d", w.Code)
	}
	j, _ = drainJob(t, s.queue)
	if j.Kind != jobAutoRev || j.PRNumber != 9 || !j.Fork {
		t.Errorf("auto-review job = %+v", j)
	}

	// Forged signature: 401, nothing parsed.
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("forged signature: status %d", rec.Code)
	}

	// Full queue: polite refusal comment.
	small := newJobQueue(1, s.runJob)
	s.queue = small
	postWebhook(t, h, env.WebhookSecret, "issue_comment", issueCommentPayload("alice", "User", "/archi design one", 42, false))
	nc = len(fg.comments)
	w = postWebhook(t, h, env.WebhookSecret, "issue_comment", issueCommentPayload("alice", "User", "/archi design two", 42, false))
	if w.Code == 202 {
		t.Error("full queue must not report 202")
	}
	if len(fg.comments) != nc+1 || !strings.Contains(fg.comments[nc], "busy") {
		t.Errorf("busy comment: %v", fg.comments[nc:])
	}
}

// ── workspace & cache ───────────────────────────────────────────────────────

func TestWorkspaceLifecycle(t *testing.T) {
	needGit(t)
	gitBase := t.TempDir()
	mkBareRepo(t, gitBase, "o/r", map[string]string{"README.md": "hi\n"})
	env := testEnv(t, "archi", gitBase)
	ctx := context.Background()

	ws1, err := prepare(ctx, job{ID: "w1", Repo: "o/r"}, "main", "", env)
	if err != nil {
		t.Fatal(err)
	}
	defer ws1.cleanup()
	if _, err := os.Stat(filepath.Join(ws1.dir, "README.md")); err != nil {
		t.Fatalf("clone missing README: %v", err)
	}
	ws2, err := prepare(ctx, job{ID: "w2", Repo: "o/r"}, "main", "", env)
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.cleanup()

	if err := os.WriteFile(filepath.Join(ws1.dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws1.commitPush(ctx, "archi/t", "first", "."); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// ws2 cloned before ws1 pushed: its push is non-fast-forward and must
	// rebase-retry.
	if err := os.WriteFile(filepath.Join(ws2.dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ws2.commitPush(ctx, "archi/t", "second", "."); err != nil {
		t.Fatalf("non-fast-forward push must rebase and retry: %v", err)
	}
	bare := filepath.Join(gitBase, "o", "r.git")
	count := strings.TrimSpace(runCmd(t, "", "git", "--git-dir", bare, "rev-list", "--count", "archi/t"))
	if count != "3" { // seed + first + second
		t.Errorf("branch commits = %s, want 3", count)
	}

	// Token never escapes runGit output.
	ws3 := &workspace{dir: ws1.dir, repo: "o/r", token: "sekret123"}
	ws3.runGit(ctx, "remote", "add", "leaky", "https://x-access-token:sekret123@example.com/o/r.git")
	out, err := ws3.runGit(ctx, "remote", "get-url", "leaky")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "sekret123") {
		t.Errorf("token leaked from runGit: %q", out)
	}
	if !strings.Contains(out, "***@") {
		t.Errorf("expected scrubbed userinfo, got %q", out)
	}
}

func TestCacheLifecycle(t *testing.T) {
	cacheDir := t.TempDir()
	wsA := &workspace{dir: t.TempDir(), repo: "o/r"}
	for rel, content := range map[string]string{
		".archi/config.json":   "{}\n",
		".archi/profile.md":    "profile\n",
		".archi/backups/b.txt": "artifact\n",
		".archi/designs/d.md":  "artifact\n",
	} {
		p := filepath.Join(wsA.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	j := job{Repo: "o/r"}
	if err := saveCache(cacheDir, j, wsA, "sha-1"); err != nil {
		t.Fatal(err)
	}
	cdir := cacheDirFor(cacheDir, "o/r")
	if _, err := os.Stat(filepath.Join(cdir, ".archi", "profile.md")); err != nil {
		t.Errorf("profile not cached: %v", err)
	}
	for _, skipped := range []string{"backups", "designs"} {
		if _, err := os.Stat(filepath.Join(cdir, ".archi", skipped)); !os.IsNotExist(err) {
			t.Errorf(".archi/%s must not be cached", skipped)
		}
	}

	wsB := &workspace{dir: t.TempDir(), repo: "o/r"}
	sha, err := restoreCache(cacheDir, j, wsB)
	if err != nil || sha != "sha-1" {
		t.Fatalf("restore: sha=%q err=%v", sha, err)
	}
	if data, err := os.ReadFile(filepath.Join(wsB.dir, ".archi", "profile.md")); err != nil || string(data) != "profile\n" {
		t.Errorf("restored profile = %q, %v", data, err)
	}

	// No cache ⇒ empty SHA, no error.
	if sha, err := restoreCache(t.TempDir(), j, wsB); sha != "" || err != nil {
		t.Errorf("no cache: sha=%q err=%v", sha, err)
	}
}

// ── pipeline end-to-end (stub archi) ────────────────────────────────────────

func stubLogLines(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestPipelineDesignAndCache(t *testing.T) {
	needGit(t)
	fg := newFakeGitHub(t)
	gitBase := t.TempDir()
	seed := mkBareRepo(t, gitBase, "o/r", map[string]string{"main.go": "package main\n"})
	stub := writeStubArchi(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("ARCHI_STUB_LOG", log)
	env := testEnv(t, stub, gitBase)
	s := newTestServer(t, fg, env)

	j := job{ID: "d1", Kind: jobDesign, InstallID: 1, Repo: "o/r", DefaultBranch: "main",
		Issue: 42, Request: "add webhooks", User: "alice"}
	s.runJob(context.Background(), j)

	if len(fg.comments) != 1 || !strings.Contains(fg.comments[0], "# Design: stub") {
		t.Fatalf("design comment: %v", fg.comments)
	}
	linesRun1 := stubLogLines(t, log)
	if len(linesRun1) != 2 || linesRun1[0] != "init" || !strings.HasPrefix(linesRun1[1], "design -no-interactive") {
		t.Fatalf("first run argv: %v", linesRun1)
	}
	if !strings.HasSuffix(linesRun1[1], " add webhooks") {
		t.Errorf("request must be the trailing argv: %q", linesRun1[1])
	}
	if _, err := os.Stat(filepath.Join(cacheDirFor(env.CacheDir, "o/r"), "head")); err != nil {
		t.Fatalf("cache not saved: %v", err)
	}

	// Second job on an unchanged repo skips init entirely (cache hit by SHA).
	j.ID = "d2"
	s.runJob(context.Background(), j)
	linesRun2 := stubLogLines(t, log)[len(linesRun1):]
	if len(linesRun2) != 1 || !strings.HasPrefix(linesRun2[0], "design") {
		t.Fatalf("second run must skip init: %v", linesRun2)
	}

	// After a default-branch push it refreshes, not full-inits.
	if err := os.WriteFile(filepath.Join(seed, "new.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, seed, "add", "-A")
	gitC(t, seed, "commit", "-m", "drift")
	gitC(t, seed, "push", "origin", "HEAD:main")
	j.ID = "d3"
	s.runJob(context.Background(), j)
	linesRun3 := stubLogLines(t, log)[len(linesRun1)+len(linesRun2):]
	if len(linesRun3) != 2 || linesRun3[0] != "init -refresh" {
		t.Fatalf("third run must refresh: %v", linesRun3)
	}
}

func TestPipelineDesignPR(t *testing.T) {
	needGit(t)
	fg := newFakeGitHub(t)
	gitBase := t.TempDir()
	mkBareRepo(t, gitBase, "o/r", map[string]string{"main.go": "package main\n"})
	stub := writeStubArchi(t)
	t.Setenv("ARCHI_STUB_LOG", filepath.Join(t.TempDir(), "argv.log"))
	env := testEnv(t, stub, gitBase)
	s := newTestServer(t, fg, env)

	j := job{ID: "p1", Kind: jobDesignPR, InstallID: 1, Repo: "o/r", DefaultBranch: "main",
		Issue: 42, Request: "add webhook delivery", User: "alice"}
	s.runJob(context.Background(), j)

	if len(fg.createdPR) != 1 {
		t.Fatalf("draft PR not created: %v; comments: %v", fg.createdPR, fg.comments)
	}
	pr := fg.createdPR[0]
	if pr["draft"] != true || pr["base"] != "main" || pr["head"] != "archi/design-42-add-webhook-delivery" {
		t.Errorf("createPR body: %+v", pr)
	}
	if title := pr["title"].(string); !strings.HasPrefix(title, "design: add webhook delivery") {
		t.Errorf("PR title: %q", title)
	}
	bare := filepath.Join(gitBase, "o", "r.git")
	doc := runCmd(t, "", "git", "--git-dir", bare, "show",
		"archi/design-42-add-webhook-delivery:docs/designs/add-webhook-delivery.md")
	if !strings.Contains(doc, "# Design: stub") {
		t.Errorf("committed design doc: %q", doc)
	}
	if len(fg.comments) != 1 || !strings.Contains(fg.comments[0], "Design ready → https://github.com/o/r/pull/43") {
		t.Errorf("issue comment: %v", fg.comments)
	}
}

func TestPipelineBuild(t *testing.T) {
	needGit(t)
	fg := newFakeGitHub(t)
	gitBase := t.TempDir()
	seed := mkBareRepo(t, gitBase, "o/r", map[string]string{"main.go": "package main\n"})
	// Seed the design branch the way a design-pr job would have.
	gitC(t, seed, "checkout", "-b", "archi/design-42-add-webhooks")
	docPath := filepath.Join(seed, "docs", "designs", "add-webhooks.md")
	os.MkdirAll(filepath.Dir(docPath), 0o755)
	if err := os.WriteFile(docPath, []byte("# The Design\n\nBuild the thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitC(t, seed, "add", "-A")
	gitC(t, seed, "commit", "-m", "design")
	gitC(t, seed, "push", "origin", "archi/design-42-add-webhooks")

	stub := writeStubArchi(t)
	log := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("ARCHI_STUB_LOG", log)
	env := testEnv(t, stub, gitBase)
	s := newTestServer(t, fg, env)

	j := job{ID: "b1", Kind: jobBuild, InstallID: 1, Repo: "o/r", DefaultBranch: "main",
		Issue: 43, PRNumber: 43, HeadRef: "archi/design-42-add-webhooks", HeadSHA: "x", BaseRef: "main", User: "alice"}
	s.runJob(context.Background(), j)

	if len(fg.comments) != 1 || !strings.Contains(fg.comments[0], "Built from the approved design") ||
		!strings.Contains(fg.comments[0], "generated.go") {
		t.Fatalf("build comment: %v", fg.comments)
	}
	// The build passed the committed design via -f, with the preamble.
	req, err := os.ReadFile(log + ".req")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(req), "Implement exactly this approved design; do not redesign.") ||
		!strings.Contains(string(req), "# The Design") {
		t.Errorf("-f request file: %q", req)
	}
	lastLine := stubLogLines(t, log)[len(stubLogLines(t, log))-1]
	if !strings.Contains(lastLine, "-build -yes -f ") {
		t.Errorf("build argv: %q", lastLine)
	}
	// The generated file landed on the SAME branch.
	bare := filepath.Join(gitBase, "o", "r.git")
	gen := runCmd(t, "", "git", "--git-dir", bare, "show", "archi/design-42-add-webhooks:generated.go")
	if gen != "generated code\n" {
		t.Errorf("generated.go on branch = %q", gen)
	}
	// The job's .archi and the design history must NOT be committed.
	tree := runCmd(t, "", "git", "--git-dir", bare, "ls-tree", "-r", "--name-only", "archi/design-42-add-webhooks")
	if strings.Contains(tree, ".archi") {
		t.Errorf(".archi leaked into the commit:\n%s", tree)
	}
	// No cache save for PR-branch jobs.
	if _, err := os.Stat(filepath.Join(cacheDirFor(env.CacheDir, "o/r"), "head")); !os.IsNotExist(err) {
		t.Error("PR-branch job must not save the shared cache")
	}
}

func TestPipelineReview(t *testing.T) {
	needGit(t)
	fg := newFakeGitHub(t)
	fg.diff = "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\n"
	gitBase := t.TempDir()
	mkBareRepo(t, gitBase, "o/r", map[string]string{"main.go": "package main\n"})
	stub := writeStubArchi(t)
	t.Setenv("ARCHI_STUB_LOG", filepath.Join(t.TempDir(), "argv.log"))
	env := testEnv(t, stub, gitBase)
	s := newTestServer(t, fg, env)

	j := job{ID: "r1", Kind: jobReview, InstallID: 1, Repo: "o/r", DefaultBranch: "main",
		Issue: 5, PRNumber: 5, HeadRef: "feature", HeadSHA: "abc123", BaseRef: "main", User: "alice"}
	s.runJob(context.Background(), j)

	if len(fg.checks) != 2 {
		t.Fatalf("want in_progress + completed check runs, got %v", fg.checks)
	}
	if fg.checks[0]["status"] != "in_progress" || fg.checks[0]["head_sha"] != "abc123" {
		t.Errorf("first check: %+v", fg.checks[0])
	}
	final := fg.checks[1]
	if final["status"] != "completed" || final["conclusion"] != "failure" {
		t.Errorf("final check: %+v", final)
	}
	output := final["output"].(map[string]any)
	if !strings.Contains(output["title"].(string), "1 blocking") {
		t.Errorf("check title: %v", output["title"])
	}
	if len(fg.reviews) != 1 || !strings.Contains(fg.reviews[0], "# archi review") {
		t.Errorf("PR review: %v", fg.reviews)
	}
}

func TestPipelineTimeout(t *testing.T) {
	needGit(t)
	fg := newFakeGitHub(t)
	gitBase := t.TempDir()
	mkBareRepo(t, gitBase, "o/r", map[string]string{"main.go": "package main\n"})
	stub := writeStubArchi(t)
	t.Setenv("ARCHI_STUB_LOG", filepath.Join(t.TempDir(), "argv.log"))
	t.Setenv("ARCHI_STUB_SLEEP", "30")
	env := testEnv(t, stub, gitBase)
	env.JobTimeout = 500 * time.Millisecond
	s := newTestServer(t, fg, env)

	j := job{ID: "t1", Kind: jobDesign, InstallID: 1, Repo: "o/r", DefaultBranch: "main",
		Issue: 42, Request: "slow", User: "alice"}
	start := time.Now()
	s.runJob(context.Background(), j)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("timeout did not kill the child promptly (took %s)", took)
	}
	if len(fg.comments) != 1 || !strings.Contains(fg.comments[0], "timed out after 500ms") {
		t.Errorf("timeout comment: %v", fg.comments)
	}
}

// ── config & misc ───────────────────────────────────────────────────────────

func TestLoadEnv(t *testing.T) {
	keyPEM, err := os.ReadFile(filepath.Join("testdata", "test_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"APP_ID":             "7",
		"WEBHOOK_SECRET":     "s",
		"PRIVATE_KEY_BASE64": base64.StdEncoding.EncodeToString(keyPEM),
	}
	getenv := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	env, err := loadEnv(getenv(base))
	if err != nil {
		t.Fatal(err)
	}
	if env.Workers != 2 || env.QueueSize != 64 || env.MaxJobsPerHour != 10 || env.JobTimeout != 15*time.Minute {
		t.Errorf("defaults: %+v", env)
	}
	if !bytes.Equal(env.PrivateKeyPEM, keyPEM) {
		t.Error("PRIVATE_KEY_BASE64 not decoded")
	}
	for _, missing := range []string{"APP_ID", "WEBHOOK_SECRET", "PRIVATE_KEY_BASE64"} {
		m := map[string]string{}
		for k, v := range base {
			if k != missing {
				m[k] = v
			}
		}
		if _, err := loadEnv(getenv(m)); err == nil {
			t.Errorf("missing %s must refuse to start", missing)
		}
	}
	bad := map[string]string{}
	for k, v := range base {
		bad[k] = v
	}
	bad["WORKERS"] = "zero"
	if _, err := loadEnv(getenv(bad)); err == nil {
		t.Error("bad WORKERS must refuse to start")
	}
	bad["WORKERS"] = "4"
	bad["JOB_TIMEOUT"] = "20m"
	env, err = loadEnv(getenv(bad))
	if err != nil || env.Workers != 4 || env.JobTimeout != 20*time.Minute {
		t.Errorf("overrides: %+v %v", env, err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		got, want string
		ok        bool
	}{
		{"1.2.0", "1.2.0", true},
		{"1.7.0", "1.2.0", true},
		{"1.10.2", "1.2.0", true},
		{"1.1.9", "1.2.0", false},
		{"0.9", "1.2.0", false},
		{"2", "1.2.0", true},
	}
	for _, c := range cases {
		if got := versionAtLeast(c.got, c.want); got != c.ok {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", c.got, c.want, got, c.ok)
		}
	}
}

func TestSlugify(t *testing.T) {
	if got := slugify("Add Webhook delivery, with retries!"); got != "add-webhook-delivery-with-retries" {
		t.Errorf("slugify = %q", got)
	}
	if got := slugify("!!!"); got != "design" {
		t.Errorf("empty slug fallback = %q", got)
	}
	if got := slugify(strings.Repeat("a", 100)); len(got) > 40 {
		t.Errorf("slug too long: %d", len(got))
	}
}
