package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ghClient is one installation-scoped GitHub REST client. Base URL is a
// field so tests point it at an httptest.Server.
type ghClient struct {
	base  string // "https://api.github.com"
	token string // installation token; sent as "Authorization: Bearer <t>"
	http  *http.Client
}

// errNotFound marks a 404 on a lookup where absence is a normal answer.
var errNotFound = errors.New("not found")

// ghError is a non-2xx API response.
type ghError struct {
	Status  int
	Message string
}

func (e *ghError) Error() string {
	return fmt.Sprintf("github: %d %s", e.Status, e.Message)
}

// do marshals in (nil OK), sends method path with the standard headers, and
// decodes a 2xx response into out (nil OK). Non-2xx returns a *ghError; a
// rate-limited 403 says so explicitly with the reset time.
func (c *ghClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(data, &e)
		msg := e.Message
		if resp.StatusCode == http.StatusForbidden && resp.Header.Get("x-ratelimit-remaining") == "0" {
			msg = fmt.Sprintf("rate limit exhausted (resets at %s) %s", resp.Header.Get("x-ratelimit-reset"), msg)
		}
		return &ghError{Status: resp.StatusCode, Message: strings.TrimSpace(fmt.Sprintf("%s %s: %s", method, path, msg))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// postComment posts an issue/PR comment and returns its id.
func (c *ghClient) postComment(ctx context.Context, repo string, issue int, body string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, issue),
		map[string]string{"body": body}, &out)
	return out.ID, err
}

// react adds the 👀 reaction to a comment — the ack that a command was seen
// and enqueued.
func (c *ghClient) react(ctx context.Context, repo string, commentID int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/comments/%d/reactions", repo, commentID),
		map[string]string{"content": "eyes"}, nil)
}

// permission returns the user's collaborator permission on the repo
// ("admin"|"maintain"|"write"|"triage"|"read"|"none"). A 404 reads as "none".
func (c *ghClient) permission(ctx context.Context, repo, user string) (string, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/collaborators/%s/permission", repo, user), nil, &out)
	var ge *ghError
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		return "none", nil
	}
	if err != nil {
		return "", err
	}
	return out.Permission, nil
}

// fileAt fetches one file's contents at a ref; 404 ⇒ errNotFound so callers
// treat absence as "use defaults".
func (c *ghClient) fileAt(ctx context.Context, repo, ref, path string) ([]byte, error) {
	var out struct {
		Content string `json:"content"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, path, ref), nil, &out)
	var ge *ghError
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

// maxDiffBytes caps how large a PR diff the app will review.
const maxDiffBytes = 5 << 20

// prDiff fetches a PR's raw unified diff via the diff media type.
func (c *ghClient) prDiff(ctx context.Context, repo string, num int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/pulls/%d", c.base, repo, num), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: fetching PR diff: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", &ghError{Status: resp.StatusCode, Message: fmt.Sprintf("fetching diff for PR #%d", num)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDiffBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxDiffBytes {
		return "", fmt.Errorf("diff too large to review (over %d MiB)", maxDiffBytes>>20)
	}
	return string(data), nil
}

// pr fetches PR head/base/draft — for /archi build and /archi review, whose
// triggering issue_comment payload has no PR details.
func (c *ghClient) pr(ctx context.Context, repo string, num int) (*prRef, error) {
	var out prRef
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, num), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// createPR opens a pull request and returns its number and URL.
func (c *ghClient) createPR(ctx context.Context, repo, title, head, base, body string, draft bool) (int, string, error) {
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls", repo), map[string]any{
		"title": title, "head": head, "base": base, "body": body, "draft": draft,
	}, &out)
	return out.Number, out.HTMLURL, err
}

// createReview posts a non-approving PR review carrying the review document.
func (c *ghClient) createReview(ctx context.Context, repo string, num int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, num),
		map[string]string{"event": "COMMENT", "body": body}, nil)
}

// createCheckRun creates a check run (a fresh "completed" run against the
// same name+SHA supersedes an earlier "in_progress" one — no PATCH needed).
func (c *ghClient) createCheckRun(ctx context.Context, repo string, cr checkRun) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/check-runs", repo), cr, nil)
}

// commentMax is the split threshold for comment bodies — a safety margin
// under GitHub's 65536-char limit.
const commentMax = 60000

// splitComment splits body into ≤max-byte parts, breaking on line boundaries
// and re-opening any fenced code block that a split lands inside. Parts 2..N
// carry a "continued" header. Pure.
func splitComment(body string, max int) []string {
	if len(body) <= max {
		return []string{body}
	}
	const reserve = 200 // header + a fence close/re-open comfortably fit
	budget := max - reserve
	if budget < 1 {
		budget = max
	}

	type part struct {
		reopen string // fence line to re-open at the top, "" for none
		lines  []string
	}
	var parts []part
	cur := part{}
	curLen := 0
	fence := "" // the open fence line, "" when outside a fence
	flush := func() {
		if fence != "" {
			cur.lines = append(cur.lines, "```") // close so the part renders
		}
		parts = append(parts, cur)
		cur = part{reopen: fence}
		curLen = 0
		if fence != "" {
			curLen = len(fence) + 1
		}
	}
	for _, line := range lines(body) {
		for len(line) > budget { // pathological single line: hard split
			cur.lines = append(cur.lines, line[:budget])
			line = line[budget:]
			flush()
		}
		if curLen+len(line)+1 > budget && len(cur.lines) > 0 {
			flush()
		}
		cur.lines = append(cur.lines, line)
		curLen += len(line) + 1
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") {
			if fence == "" {
				fence = t
			} else {
				fence = ""
			}
		}
	}
	parts = append(parts, cur)

	out := make([]string, 0, len(parts))
	for i, p := range parts {
		var b strings.Builder
		if i > 0 {
			fmt.Fprintf(&b, "_…continued (part %d/%d)_\n\n", i+1, len(parts))
			if p.reopen != "" {
				b.WriteString(p.reopen + "\n")
			}
		}
		b.WriteString(strings.Join(p.lines, "\n"))
		out = append(out, b.String())
	}
	return out
}

// lines splits on '\n' without dropping a trailing newline's empty tail.
func lines(s string) []string { return strings.Split(s, "\n") }
