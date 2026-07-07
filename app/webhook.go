package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// verifySignature checks X-Hub-Signature-256 ("sha256=" + hex HMAC-SHA256 of
// the RAW body with the webhook secret) in constant time. Missing header,
// bad prefix, or bad hex all read as unverified. Pure.
func verifySignature(secret, body []byte, sigHeader string) bool {
	hexMAC, ok := strings.CutPrefix(sigHeader, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexMAC)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Minimal payload mirrors — only the fields the app reads. Unknown fields
// are ignored by design (plain json.Unmarshal).

type issueCommentEvent struct {
	Action  string `json:"action"` // only "created" is handled
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
			Type  string `json:"type"` // "Bot" comments are ignored (loop guard)
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number      int              `json:"number"`
		PullRequest *json.RawMessage `json:"pull_request"` // non-nil ⇒ comment on a PR
	} `json:"issue"`
	Repository   repoRef `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type pullRequestEvent struct {
	Action       string  `json:"action"` // "opened" | "synchronize" handled
	Number       int     `json:"number"`
	PullRequest  prRef   `json:"pull_request"`
	Repository   repoRef `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type repoRef struct {
	FullName      string `json:"full_name"` // "owner/repo"
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

type prRef struct {
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Draft bool `json:"draft"`
}

// isFork reports whether the PR head lives in a different repo than the base.
func isFork(pr prRef) bool {
	return pr.Head.Repo.FullName != pr.Base.Repo.FullName
}

// handleWebhook verifies and routes one delivery. It always answers fast:
// 202 when a job was enqueued, 200 for everything else understood, 401 for a
// bad signature — all real work happens on the queue.
func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if !verifySignature([]byte(s.env.WebhookSecret), body, r.Header.Get("X-Hub-Signature-256")) {
		logf("warn", "bad webhook signature", "delivery", delivery)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	switch event := r.Header.Get("X-GitHub-Event"); event {
	case "ping":
		var p struct {
			Zen string `json:"zen"`
		}
		json.Unmarshal(body, &p)
		logf("info", "ping", "zen", p.Zen)
		fmt.Fprint(w, "pong")
	case "issue_comment":
		s.handleIssueComment(r.Context(), w, body, delivery)
	case "pull_request":
		s.handlePullRequest(r.Context(), w, body, delivery)
	case "installation":
		var e struct {
			Action       string `json:"action"`
			Installation struct {
				ID      int64 `json:"id"`
				Account struct {
					Login string `json:"login"`
				} `json:"account"`
			} `json:"installation"`
		}
		json.Unmarshal(body, &e)
		logf("info", "installation event", "action", e.Action, "install", e.Installation.ID, "account", e.Installation.Account.Login)
		if e.Action == "deleted" {
			s.auth.dropInstallation(e.Installation.ID)
		}
		w.WriteHeader(http.StatusOK)
	case "issues":
		w.WriteHeader(http.StatusOK) // subscribed for future issue-body triggers
	default:
		logf("info", "event ignored", "event", event, "delivery", delivery)
		w.WriteHeader(http.StatusOK)
	}
}

// handleIssueComment turns a "/archi …" comment into a job: parse →
// permission gate → rate gate → 👀 ack → enqueue. Refusals are reply
// comments; non-commands are silent.
func (s *server) handleIssueComment(ctx context.Context, w http.ResponseWriter, body []byte, delivery string) {
	var e issueCommentEvent
	if err := json.Unmarshal(body, &e); err != nil {
		logf("warn", "bad issue_comment payload", "delivery", delivery, "err", err.Error())
		w.WriteHeader(http.StatusOK)
		return
	}
	if e.Action != "created" || e.Comment.User.Type == "Bot" {
		w.WriteHeader(http.StatusOK)
		return
	}
	cmd, ok := parseCommand(e.Comment.Body)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	repo, issue := e.Repository.FullName, e.Issue.Number
	gh, err := s.client(ctx, e.Installation.ID)
	if err != nil {
		logf("error", "installation token", "repo", repo, "err", err.Error())
		w.WriteHeader(http.StatusOK)
		return
	}
	reply := func(msg string) {
		if _, err := gh.postComment(ctx, repo, issue, msg); err != nil {
			logf("error", "posting comment", "repo", repo, "err", err.Error())
		}
		w.WriteHeader(http.StatusOK)
	}

	if cmd.Name == "help" {
		msg := helpComment
		if cmd.helpNote != "" {
			msg = "> " + cmd.helpNote + "\n\n" + msg
		}
		reply(msg)
		return
	}

	// Permission gate — before any enqueue, clone, or LLM call.
	perm, err := gh.permission(ctx, repo, e.Comment.User.Login)
	if err != nil {
		logf("error", "permission check", "repo", repo, "user", e.Comment.User.Login, "err", err.Error())
		w.WriteHeader(http.StatusOK)
		return
	}
	if perm != "write" && perm != "maintain" && perm != "admin" {
		reply(fmt.Sprintf("@%s you need write access to run archi commands.", e.Comment.User.Login))
		return
	}

	// Rate gate — checked at enqueue time so the refusal is immediate.
	if !s.queue.gateFor(repo).allow(s.now(), s.env.MaxJobsPerHour) {
		reply(fmt.Sprintf("This repo is over archi's rate limit (%d jobs/hour) — try again within the hour.", s.env.MaxJobsPerHour))
		return
	}

	j := job{
		ID:            newJobID(),
		InstallID:     e.Installation.ID,
		Repo:          repo,
		DefaultBranch: e.Repository.DefaultBranch,
		Issue:         issue,
		Request:       cmd.Request,
		User:          e.Comment.User.Login,
	}
	onPR := e.Issue.PullRequest != nil
	switch cmd.Name {
	case "design":
		j.Kind = jobDesign
		if cmd.PR {
			j.Kind = jobDesignPR
		}
	case "build", "review":
		if !onPR {
			reply(fmt.Sprintf("`/archi %s` only works on a pull request.", cmd.Name))
			return
		}
		pr, err := gh.pr(ctx, repo, issue)
		if err != nil {
			reply(fmt.Sprintf("could not load PR #%d: %v", issue, err))
			return
		}
		j.PRNumber = issue
		j.HeadRef, j.HeadSHA, j.BaseRef = pr.Head.Ref, pr.Head.SHA, pr.Base.Ref
		j.Fork = isFork(*pr)
		if cmd.Name == "review" {
			j.Kind = jobReview
		} else {
			j.Kind = jobBuild
			if j.Fork {
				reply("`/archi build` doesn't run on fork PRs — archi can't push to a fork branch, and building fork-authored designs into this repo would be unsafe. Re-create the design with `/archi design --pr …` here instead.")
				return
			}
			if !strings.HasPrefix(j.HeadRef, "archi/design-") {
				reply("this isn't an archi design PR — start with `/archi design --pr <request>` on an issue.")
				return
			}
		}
	}

	if err := gh.react(ctx, repo, e.Comment.ID); err != nil {
		logf("warn", "reaction failed", "repo", repo, "err", err.Error())
	}
	if err := s.queue.enqueue(j); err != nil {
		reply("archi is busy — try again in a few minutes.")
		return
	}
	logf("info", "job enqueued", "job", j.ID, "kind", string(j.Kind), "repo", repo, "issue", issue, "user", j.User)
	w.WriteHeader(http.StatusAccepted)
}

// handlePullRequest enqueues an auto-review when the repo opted in via
// .github/archi.yml. Repo config is the authorization — no permission check,
// and the ack is the in_progress check run the pipeline posts.
func (s *server) handlePullRequest(ctx context.Context, w http.ResponseWriter, body []byte, delivery string) {
	var e pullRequestEvent
	if err := json.Unmarshal(body, &e); err != nil {
		logf("warn", "bad pull_request payload", "delivery", delivery, "err", err.Error())
		w.WriteHeader(http.StatusOK)
		return
	}
	if e.Action != "opened" && e.Action != "synchronize" {
		w.WriteHeader(http.StatusOK)
		return
	}
	repo := e.Repository.FullName
	gh, err := s.client(ctx, e.Installation.ID)
	if err != nil {
		logf("error", "installation token", "repo", repo, "err", err.Error())
		w.WriteHeader(http.StatusOK)
		return
	}
	cfg, _ := repoConfigFor(ctx, gh, repo, e.Repository.DefaultBranch)
	if !cfg.AutoReview {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !s.queue.gateFor(repo).allow(s.now(), s.env.MaxJobsPerHour) {
		logf("warn", "auto-review skipped: rate limit", "repo", repo, "pr", e.Number)
		w.WriteHeader(http.StatusOK)
		return
	}
	j := job{
		ID:            newJobID(),
		Kind:          jobAutoRev,
		InstallID:     e.Installation.ID,
		Repo:          repo,
		DefaultBranch: e.Repository.DefaultBranch,
		Issue:         e.Number,
		PRNumber:      e.Number,
		HeadRef:       e.PullRequest.Head.Ref,
		HeadSHA:       e.PullRequest.Head.SHA,
		BaseRef:       e.PullRequest.Base.Ref,
		Fork:          isFork(e.PullRequest),
	}
	if err := s.queue.enqueue(j); err != nil {
		logf("warn", "auto-review skipped: queue full", "repo", repo, "pr", e.Number)
		w.WriteHeader(http.StatusOK)
		return
	}
	logf("info", "job enqueued", "job", j.ID, "kind", string(j.Kind), "repo", repo, "pr", e.Number)
	w.WriteHeader(http.StatusAccepted)
}
