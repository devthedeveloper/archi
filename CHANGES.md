# Technical-gap work — 2026-07-06

All changes are stdlib-only (go.mod still has zero dependencies). Build, vet,
gofmt, and the full test suite pass. Four agents worked the gaps in two waves.

## 1. Provider resilience + OpenAI-compatible backend
- `retry.go` (new): shared retry layer — 429/5xx and transient network errors
  retried up to 3× with jittered exponential backoff (1s/2s/4s), `Retry-After`
  honored (capped 30s). Never retries once a stream has started.
- Timeouts: flat 5-minute client timeout replaced with a 60s
  response-header timeout + dial/TLS timeouts; `ARCHI_TIMEOUT` env (Go
  duration) sets an overall per-request deadline.
- `openai.go` (new): OpenAI Chat Completions streaming provider.
  `OPENAI_API_KEY` + `OPENAI_BASE_URL` — the base URL override covers
  OpenRouter, Groq, and any OpenAI-compatible endpoint.

## 2. Scan safety
- Nested `.gitignore` support: subdirectory ignore files apply with correct
  git scoping/negation semantics (previously only the root file was read).
- `secrets.go` (new): likely secrets (AWS/GitHub/Slack/Stripe keys, private
  key blocks, bearer tokens, generic `api_key=`-style assignments) are
  redacted before any file content reaches an LLM prompt, with a stderr
  warning. Obvious secret files (`.env`, `*.pem`, `*.key`, `id_rsa*`) are
  excluded from scanning entirely.

## 3. Build safety + design history
- Versioned backups: overwrites/deletes now copy originals to
  `.archi/backups/<timestamp>/<relpath>` with a manifest; last 10 runs kept.
  (Replaces the single-generation `<file>.bak`.)
- `diff.go` (new): colored unified diff of every overwrite shown in the
  proposed-changes preview before you confirm.
- `history.go` (new): every design saved to `.archi/designs/<ts>-<slug>.md`
  with request/provider/date/built metadata; `archi status` lists the last 5.
- Dirty-repo warning before applying (via `git status --porcelain`), non-blocking.

## 4. Cache freshness + context budgeting
- `fingerprint.go` (new): stat-only repo fingerprint stored at
  `.archi/fingerprint`. `archi design` warns when the repo has drifted since
  init; `archi status` shows a freshness line + cache age.
- `archi init -refresh`: incremental profile update — re-sends only
  added/changed files against the existing profile; falls back to a full
  rebuild past 40% drift.
- `budget.go` (new): grounding context (profile/map/focus) fitted to a token
  budget (48k default, `ARCHI_CONTEXT_TOKENS` override) with warnings for
  anything truncated or dropped.

## Verification
- `gofmt -l` clean · `go vet` clean · `go test ./... -count=1` ok ·
  binary builds and runs (`archi version`, guard rails verified on an
  uninitialized directory).
- Codebase: 4,875 lines across 27 Go files (was ~2,550 / 20).

## Deferred (needs product decisions)
- Retrieval/embeddings for very large monorepos (budgeting mitigates for now)
- Config file (`~/.config/archi`) — trivial, but format worth deciding first
- Git-branch-based apply (writes to a branch instead of the working tree)
