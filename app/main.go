// Command archi-app is the archi GitHub App: a webhook service that runs the
// archi CLI on issue-comment commands (/archi design, /archi build,
// /archi review) and posts the results back as comments, draft PRs, PR
// reviews, and check runs. It shells out to the CLI — one child process per
// job in a throwaway workspace — rather than importing it.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is stamped by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

// minCLIVersion is the oldest archi CLI this app drives correctly (needs
// `design -no-interactive`, `-build -yes`, `init -refresh`, `review -patch`).
const minCLIVersion = "1.2.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		return
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "version", "-version", "--version", "-v":
		fmt.Println("archi-app", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `usage: archi-app <command>

  serve     Run the GitHub App webhook service (flags: -addr :8443)
  version   Print the version

Environment: APP_ID, WEBHOOK_SECRET, PRIVATE_KEY_PATH or PRIVATE_KEY_BASE64
(required); ARCHI_BIN, CACHE_DIR, WORK_DIR, WORKERS, QUEUE_SIZE,
MAX_JOBS_PER_HOUR, JOB_TIMEOUT, provider API keys (optional).
`)
}

// server wires the long-lived pieces together.
type server struct {
	env     envConfig
	auth    *appAuth
	queue   *jobQueue
	now     func() time.Time // injectable clock for tests
	apiBase string           // "https://api.github.com"; httptest in tests
	httpc   *http.Client
}

// client builds an installation-scoped REST client.
func (s *server) client(ctx context.Context, installID int64) (*ghClient, error) {
	token, err := s.auth.installationToken(ctx, installID)
	if err != nil {
		return nil, err
	}
	return &ghClient{base: s.apiBase, token: token, http: s.httpc}, nil
}

// runServe parses flags, loads the environment, probes the archi binary, and
// serves webhooks until SIGINT/SIGTERM. TLS is the proxy's job — -addr
// serves plain HTTP behind Fly/Cloud Run/nginx.
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8443", "listen address")
	fs.Parse(args)

	env, err := loadEnv(os.Getenv)
	if err != nil {
		fatalf("config: %v", err)
	}
	if err := probeCLI(env.ArchiBin); err != nil {
		fatalf("archi CLI: %v", err)
	}
	key, err := loadPrivateKey(env.PrivateKeyPEM)
	if err != nil {
		fatalf("%v", err)
	}

	httpc := &http.Client{Timeout: 30 * time.Second}
	s := &server{
		env: env,
		auth: &appAuth{
			appID: env.AppID, key: key, base: "https://api.github.com",
			http: httpc, now: time.Now, cache: map[int64]instToken{},
		},
		now:     time.Now,
		apiBase: "https://api.github.com",
		httpc:   httpc,
	}
	s.queue = newJobQueue(env.QueueSize, s.runJob)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s.queue.start(ctx, env.Workers)
	go evictLoop(ctx, env.CacheDir)

	srv := &http.Server{Addr: *addr, Handler: s.handler()}
	go func() {
		<-ctx.Done()
		shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shctx)
	}()
	logf("info", "listening", "addr", *addr, "version", version, "workers", env.Workers)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatalf("serve: %v", err)
	}
}

// handler routes: POST /webhook, GET /healthz (liveness only — checks
// nothing external), 404 for everything else.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.handleWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	return mux
}

// evictLoop prunes repo caches unused for >30 days, at startup and daily.
func evictLoop(ctx context.Context, cacheDir string) {
	const maxAge = 30 * 24 * time.Hour
	evictCaches(cacheDir, time.Now(), maxAge)
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			evictCaches(cacheDir, now, maxAge)
		}
	}
}

// probeCLI verifies the archi binary runs and is new enough to drive.
func probeCLI(bin string) error {
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		return fmt.Errorf("%q is not runnable: %v (set ARCHI_BIN or install archi on PATH)", bin, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return fmt.Errorf("unexpected `%s version` output %q", bin, strings.TrimSpace(string(out)))
	}
	got := fields[len(fields)-1]
	if !versionAtLeast(got, minCLIVersion) {
		return fmt.Errorf("archi %s is too old — need >= %s", got, minCLIVersion)
	}
	return nil
}

// versionAtLeast compares dotted numeric versions ("1.10.2" >= "1.2.0").
// Non-numeric segments compare as zero.
func versionAtLeast(got, want string) bool {
	g, w := strings.Split(got, "."), strings.Split(want, ".")
	for i := 0; i < len(g) || i < len(w); i++ {
		gi, wi := 0, 0
		if i < len(g) {
			gi, _ = strconv.Atoi(g[i])
		}
		if i < len(w) {
			wi, _ = strconv.Atoi(w[i])
		}
		if gi != wi {
			return gi > wi
		}
	}
	return true
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "archi-app: "+format+"\n", a...)
	os.Exit(1)
}

// logMu keeps concurrent workers' log lines whole.
var logMu sync.Mutex

// logf writes one structured line to stderr: time=… level=… msg=… k=v ….
// Values are %q-quoted. Never pass tokens to logf — git output is scrubbed
// by redactGit before it can reach here.
func logf(level, msg string, kv ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "time=%s level=%s msg=%q", time.Now().UTC().Format(time.RFC3339), level, msg)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %v=%q", kv[i], fmt.Sprint(kv[i+1]))
	}
	fmt.Fprintln(os.Stderr, b.String())
}
