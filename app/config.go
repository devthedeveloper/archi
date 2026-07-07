package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// envConfig is the app's process configuration (README §env table). It is
// loaded once at startup; a missing required value refuses to start.
type envConfig struct {
	AppID, WebhookSecret        string
	PrivateKeyPEM               []byte // from PRIVATE_KEY_BASE64 (wins) or PRIVATE_KEY_PATH
	ArchiBin, CacheDir, WorkDir string
	Workers, QueueSize          int
	MaxJobsPerHour              int
	JobTimeout                  time.Duration

	// gitBase is where clones come from: "https://github.com" in production,
	// a local directory of bare repos in tests.
	gitBase string
}

// loadEnv reads and validates the environment. getenv is injectable so tests
// never touch the real process env.
func loadEnv(getenv func(string) string) (envConfig, error) {
	e := envConfig{
		AppID:          getenv("APP_ID"),
		WebhookSecret:  getenv("WEBHOOK_SECRET"),
		ArchiBin:       orDefault(getenv("ARCHI_BIN"), "archi"),
		CacheDir:       orDefault(getenv("CACHE_DIR"), "/var/lib/archi-app"),
		WorkDir:        orDefault(getenv("WORK_DIR"), os.TempDir()),
		Workers:        2,
		QueueSize:      64,
		MaxJobsPerHour: 10,
		JobTimeout:     15 * time.Minute,
		gitBase:        "https://github.com",
	}
	if e.AppID == "" {
		return e, fmt.Errorf("APP_ID is required")
	}
	if e.WebhookSecret == "" {
		return e, fmt.Errorf("WEBHOOK_SECRET is required")
	}
	switch b64, path := getenv("PRIVATE_KEY_BASE64"), getenv("PRIVATE_KEY_PATH"); {
	case b64 != "": // wins over PATH, for env-only platforms
		pem, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return e, fmt.Errorf("PRIVATE_KEY_BASE64: %v", err)
		}
		e.PrivateKeyPEM = pem
	case path != "":
		pem, err := os.ReadFile(path)
		if err != nil {
			return e, fmt.Errorf("PRIVATE_KEY_PATH: %v", err)
		}
		e.PrivateKeyPEM = pem
	default:
		return e, fmt.Errorf("one of PRIVATE_KEY_BASE64 or PRIVATE_KEY_PATH is required")
	}
	for _, v := range []struct {
		name string
		dst  *int
	}{
		{"WORKERS", &e.Workers},
		{"QUEUE_SIZE", &e.QueueSize},
		{"MAX_JOBS_PER_HOUR", &e.MaxJobsPerHour},
	} {
		if s := getenv(v.name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return e, fmt.Errorf("%s must be a positive integer, got %q", v.name, s)
			}
			*v.dst = n
		}
	}
	if s := getenv("JOB_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return e, fmt.Errorf("JOB_TIMEOUT must be a positive Go duration, got %q", s)
		}
		e.JobTimeout = d
	}
	return e, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// repoConfig mirrors .github/archi.yml. The zero value means "all defaults":
// provider/model from the CLI or cache, agents full, auto_review off,
// max_tokens 8000 — the pipeline omits flags for zero fields so the CLI's own
// defaults apply.
type repoConfig struct {
	Provider    string
	Model       string
	CriticModel string
	Agents      string // "full" | "fast"
	AutoReview  bool
	MaxTokens   int
}

// parseRepoConfig parses the FLAT SUBSET of YAML the app supports: one
// "key: value" per line, "#" comments (whole-line or trailing), blank lines
// ignored. Bad values and unknown keys produce warnings (posted once in the
// job's result comment) and are otherwise skipped; nested YAML is refused
// with a warning. Pure.
func parseRepoConfig(data []byte) (repoConfig, []string) {
	var rc repoConfig
	var warns []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Strip a trailing comment (YAML requires whitespace before '#').
		if i := strings.Index(trimmed, " #"); i >= 0 {
			trimmed = strings.TrimSpace(trimmed[:i])
		}
		nested := line != strings.TrimLeft(line, " \t") || // indented
			strings.HasSuffix(trimmed, ":") || // "key:" opening a block
			strings.HasPrefix(trimmed, "- ") // list item
		if nested {
			warns = append(warns, fmt.Sprintf("archi.yml line %q: full YAML is not supported — flat key: value only", trimmed))
			continue
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			warns = append(warns, fmt.Sprintf("archi.yml line %q: expected key: value", trimmed))
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "provider":
			rc.Provider = value
		case "model":
			rc.Model = value
		case "critic_model":
			rc.CriticModel = value
		case "agents":
			if value != "full" && value != "fast" {
				warns = append(warns, fmt.Sprintf("archi.yml: agents must be full or fast, got %q", value))
				continue
			}
			rc.Agents = value
		case "auto_review":
			switch value {
			case "true":
				rc.AutoReview = true
			case "false":
				rc.AutoReview = false
			default:
				warns = append(warns, fmt.Sprintf("archi.yml: auto_review must be true or false, got %q", value))
			}
		case "max_tokens":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				warns = append(warns, fmt.Sprintf("archi.yml: max_tokens must be a positive integer, got %q", value))
				continue
			}
			rc.MaxTokens = n
		default:
			warns = append(warns, fmt.Sprintf("archi.yml: unknown key %q ignored", key))
		}
	}
	return rc, warns
}

// repoConfigFor fetches .github/archi.yml at ref; an absent file means pure
// defaults with no warning, any other fetch error is downgraded to a warning
// so config trouble never blocks a job.
func repoConfigFor(ctx context.Context, c *ghClient, repo, ref string) (repoConfig, []string) {
	data, err := c.fileAt(ctx, repo, ref, ".github/archi.yml")
	if err == errNotFound {
		return repoConfig{}, nil
	}
	if err != nil {
		return repoConfig{}, []string{fmt.Sprintf("could not read .github/archi.yml: %v", err)}
	}
	return parseRepoConfig(data)
}
