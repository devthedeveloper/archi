package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// workspace is one job's working clone. The token appears only in the remote
// URL and is scrubbed from every output before it can reach an error, log
// line, or comment.
type workspace struct {
	dir   string // <WORK_DIR>/archi-app/<job.ID>
	repo  string
	token string
}

// cloneURL builds the fetch/push URL: token-authenticated HTTPS against a
// real forge, a plain path join when gitBase is a local directory of bare
// repos (tests).
func cloneURL(gitBase, repo, token string) string {
	if strings.HasPrefix(gitBase, "http://") || strings.HasPrefix(gitBase, "https://") {
		u, err := url.Parse(gitBase)
		if err != nil {
			return gitBase + "/" + repo + ".git"
		}
		return u.Scheme + "://x-access-token:" + token + "@" + u.Host + "/" + repo + ".git"
	}
	return filepath.Join(gitBase, filepath.FromSlash(repo)+".git")
}

// prepare shallow-clones the repo at ref into a fresh directory and sets the
// bot identity on the clone. Depth 50 (not 1) so build commits on existing
// branches don't fight shallow-push edge cases. The job's .archi/ artifacts
// are excluded from git via .git/info/exclude so `git add -A` never commits
// them.
func prepare(ctx context.Context, j job, ref, token string, env envConfig) (*workspace, error) {
	dir := filepath.Join(env.WorkDir, "archi-app", j.ID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	w := &workspace{dir: dir, repo: j.Repo, token: token}
	clone := exec.CommandContext(ctx, "git", "clone", "--depth", "50", "--branch", ref,
		"--single-branch", cloneURL(env.gitBase, j.Repo, token), dir)
	if out, err := clone.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone %s@%s: %s", j.Repo, ref, redactGit(string(out), token))
	}
	if _, err := w.runGit(ctx, "config", "user.name", "archi[bot]"); err != nil {
		return nil, err
	}
	email := env.AppID + "+archi[bot]@users.noreply.github.com"
	if _, err := w.runGit(ctx, "config", "user.email", email); err != nil {
		return nil, err
	}
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	if f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.WriteString("\n.archi/\npr.diff\n")
		f.Close()
	}
	return w, nil
}

// runGit runs git with argv (never a shell), CWD w.dir, and returns combined
// output with the token scrubbed before it can reach anything user-visible.
func (w *workspace) runGit(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = w.dir
	out, err := cmd.CombinedOutput()
	safe := redactGit(string(out), w.token)
	if err != nil {
		return safe, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(safe))
	}
	return safe, nil
}

// tokenUserinfoRe matches any x-access-token userinfo in a URL, so a token
// is scrubbed even when it isn't the one we know about.
var tokenUserinfoRe = regexp.MustCompile(`x-access-token:[^@\s]*@`)

// redactGit replaces every occurrence of the token (and any x-access-token
// URL userinfo) with "***". Pure.
func redactGit(out, token string) string {
	if token != "" {
		out = strings.ReplaceAll(out, token, "***")
	}
	return tokenUserinfoRe.ReplaceAllString(out, "***@")
}

// outputCap bounds captured CLI stdout/stderr.
const outputCap = 2 << 20

// cappedBuffer keeps the first max bytes written and drops the rest.
type cappedBuffer struct {
	max int
	b   bytes.Buffer
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

// runArchi executes the CLI with the request as one argv element — never a
// shell. HOME points inside the workspace so nothing global is touched, and
// the child env carries only PATH, provider keys, and ARCHI_* vars. The exit
// code is returned separately; err is reserved for failures to run at all.
func (w *workspace) runArchi(ctx context.Context, env envConfig, args ...string) (stdout, stderr string, exit int, err error) {
	cmd := exec.CommandContext(ctx, env.ArchiBin, args...)
	cmd.Dir = w.dir
	cmd.Env = append(archiEnv(os.Environ(), env.JobTimeout), "HOME="+w.dir)
	so := &cappedBuffer{max: outputCap}
	se := &cappedBuffer{max: outputCap}
	cmd.Stdout, cmd.Stderr = so, se
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		exit, err = ee.ExitCode(), nil
	}
	return so.b.String(), se.b.String(), exit, err
}

// archiEnv builds the child env from base: PATH and provider keys are
// copied, every ARCHI_* var passes through verbatim, everything else is
// dropped, and ARCHI_TIMEOUT is pinned to the job timeout so the CLI's own
// request cap agrees with ours. Pure.
func archiEnv(base []string, timeout time.Duration) []string {
	keep := map[string]bool{
		"PATH":              true,
		"ANTHROPIC_API_KEY": true,
		"OLLAMA_API_KEY":    true,
		"OLLAMA_HOST":       true,
		"OPENAI_API_KEY":    true,
		"OPENAI_BASE_URL":   true,
	}
	var out []string
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "ARCHI_TIMEOUT" || k == "HOME" {
			continue
		}
		if keep[k] || strings.HasPrefix(k, "ARCHI_") {
			out = append(out, kv)
		}
	}
	return append(out, "ARCHI_TIMEOUT="+timeout.String())
}

// ---- .archi cache: survives the workspace, keyed by default-branch SHA ----

// cacheDirFor returns <cacheDir>/repos/<owner>__<repo>, which holds a full
// .archi/ tree plus a "head" file recording the SHA it matches.
func cacheDirFor(cacheDir, repo string) string {
	return filepath.Join(cacheDir, "repos", strings.ReplaceAll(repo, "/", "__"))
}

// restoreCache copies the cached .archi/ into the workspace and returns the
// recorded SHA ("" when no cache). A copy failure means a corrupt cache: the
// cache dir is deleted and the job proceeds cacheless.
func restoreCache(cacheDir string, j job, ws *workspace) (string, error) {
	cdir := cacheDirFor(cacheDir, j.Repo)
	shaBytes, err := os.ReadFile(filepath.Join(cdir, "head"))
	if err != nil {
		return "", nil // no cache
	}
	if err := copyTree(filepath.Join(cdir, ".archi"), filepath.Join(ws.dir, ".archi"), nil); err != nil {
		logf("warn", "corrupt cache dropped", "repo", j.Repo, "err", err.Error())
		os.RemoveAll(cdir)
		os.RemoveAll(filepath.Join(ws.dir, ".archi"))
		return "", nil
	}
	return strings.TrimSpace(string(shaBytes)), nil
}

// saveCache copies the workspace .archi/ back, minus backups/ and designs/
// (job artifacts, not shared understanding), and writes "head" LAST so a
// crash mid-save reads as stale (refresh next time), never corrupt-but-fresh.
func saveCache(cacheDir string, j job, ws *workspace, sha string) error {
	cdir := cacheDirFor(cacheDir, j.Repo)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		return err
	}
	os.Remove(filepath.Join(cdir, "head")) // invalidate first: crash ⇒ stale, not wrong
	dst := filepath.Join(cdir, ".archi")
	os.RemoveAll(dst)
	if err := copyTree(filepath.Join(ws.dir, ".archi"), dst, map[string]bool{"backups": true, "designs": true}); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cdir, "head"), []byte(sha+"\n"), 0o644)
}

// copyTree copies src into dst recursively, skipping top-level entries named
// in skipTop.
func copyTree(src, dst string, skipTop map[string]bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel != "." {
			top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
			if skipTop[top] {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// evictCaches deletes repo cache dirs untouched for longer than maxAge.
// Best-effort; called at startup and daily.
func evictCaches(cacheDir string, now time.Time, maxAge time.Duration) {
	repos := filepath.Join(cacheDir, "repos")
	entries, err := os.ReadDir(repos)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			os.RemoveAll(filepath.Join(repos, e.Name()))
			logf("info", "cache evicted", "repo", e.Name())
		}
	}
}

// commitPush stages paths, commits, and pushes the branch, retrying once
// after a rebase when the remote moved. The second failure surfaces so the
// pipeline can say "branch moved — re-run".
func (w *workspace) commitPush(ctx context.Context, branch, msg string, paths ...string) error {
	if _, err := w.runGit(ctx, "checkout", "-B", branch); err != nil {
		return err
	}
	if _, err := w.runGit(ctx, append([]string{"add", "-A"}, paths...)...); err != nil {
		return err
	}
	if _, err := w.runGit(ctx, "commit", "-m", msg); err != nil {
		return err
	}
	if _, err := w.runGit(ctx, "push", "origin", branch); err == nil {
		return nil
	}
	if _, err := w.runGit(ctx, "pull", "--rebase", "origin", branch); err != nil {
		return err
	}
	if _, err := w.runGit(ctx, "push", "origin", branch); err != nil {
		return fmt.Errorf("branch moved while pushing: %v", err)
	}
	return nil
}

// cleanup removes the workspace directory; the cache dir survives.
func (w *workspace) cleanup() { os.RemoveAll(w.dir) }
