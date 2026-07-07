package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"gopherit/internal/store"
)

// newTestServer spins up the whole app against a throw-away database.
// t.TempDir() is deleted automatically when the test finishes.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	app := &application{
		config: config{env: "test"},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), // silence logs
		store:  st,
	}

	ts := httptest.NewServer(app.routes())
	t.Cleanup(ts.Close)
	return ts
}

// do sends a JSON request and decodes the JSON response into a map.
func do(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}

	req, err := http.NewRequest(method, ts.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response from %s %s: %v", method, path, err)
	}
	return res.StatusCode, got
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)

	status, got := do(t, ts, "GET", "/api/v1/healthz", "", nil)
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if got["status"] != "available" {
		t.Fatalf(`want status "available", got %v`, got["status"])
	}
}

// TestHappyPath walks the whole gopherit story: register, log in, create
// a community, post, comment, reply and vote.
func TestHappyPath(t *testing.T) {
	ts := newTestServer(t)

	// Register.
	status, got := do(t, ts, "POST", "/api/v1/users", "", map[string]any{
		"username": "gopher",
		"email":    "gopher@example.com",
		"password": "secret-password",
	})
	if status != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%v)", status, got)
	}

	// Duplicate registration must be rejected.
	status, _ = do(t, ts, "POST", "/api/v1/users", "", map[string]any{
		"username": "gopher",
		"email":    "gopher@example.com",
		"password": "secret-password",
	})
	if status != http.StatusConflict {
		t.Fatalf("duplicate register: want 409, got %d", status)
	}

	// Log in.
	status, got = do(t, ts, "POST", "/api/v1/tokens", "", map[string]any{
		"email":    "gopher@example.com",
		"password": "secret-password",
	})
	if status != http.StatusCreated {
		t.Fatalf("login: want 201, got %d (%v)", status, got)
	}
	token, _ := got["token"].(string)
	if token == "" {
		t.Fatal("login response did not include a token")
	}

	// Creating a subreddit without a token must fail.
	status, _ = do(t, ts, "POST", "/api/v1/subreddits", "", map[string]any{
		"name": "golang", "title": "The Go community",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous create subreddit: want 401, got %d", status)
	}

	// With the token it succeeds.
	status, got = do(t, ts, "POST", "/api/v1/subreddits", token, map[string]any{
		"name": "golang", "title": "The Go community", "description": "Gophers welcome",
	})
	if status != http.StatusCreated {
		t.Fatalf("create subreddit: want 201, got %d (%v)", status, got)
	}

	// Create a post.
	status, got = do(t, ts, "POST", "/api/v1/posts", token, map[string]any{
		"subreddit": "golang",
		"title":     "I built a Reddit clone in Go!",
		"body":      "And so can you.",
	})
	if status != http.StatusCreated {
		t.Fatalf("create post: want 201, got %d (%v)", status, got)
	}
	post := got["post"].(map[string]any)
	postID := int64(post["id"].(float64))

	// Comment on it, then reply to that comment.
	status, got = do(t, ts, "POST", "/api/v1/posts/1/comments", token, map[string]any{
		"body": "Nice work!",
	})
	if status != http.StatusCreated {
		t.Fatalf("create comment: want 201, got %d (%v)", status, got)
	}
	comment := got["comment"].(map[string]any)
	commentID := int64(comment["id"].(float64))

	status, _ = do(t, ts, "POST", "/api/v1/posts/1/comments", token, map[string]any{
		"body": "Thanks!", "parent_id": commentID,
	})
	if status != http.StatusCreated {
		t.Fatalf("create reply: want 201, got %d", status)
	}

	// Upvote the post.
	status, got = do(t, ts, "POST", "/api/v1/posts/1/vote", token, map[string]any{
		"value": 1,
	})
	if status != http.StatusOK {
		t.Fatalf("vote: want 200, got %d (%v)", status, got)
	}
	if got["score"].(float64) != 1 {
		t.Fatalf("want score 1, got %v", got["score"])
	}

	// Voting again with the same value must not double-count.
	status, got = do(t, ts, "POST", "/api/v1/posts/1/vote", token, map[string]any{
		"value": 1,
	})
	if status != http.StatusOK || got["score"].(float64) != 1 {
		t.Fatalf("repeat vote: want 200/score 1, got %d/%v", status, got["score"])
	}

	// The post shows up in the feed with its comments counted.
	status, got = do(t, ts, "GET", "/api/v1/posts?sort=hot", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list posts: want 200, got %d", status)
	}
	posts := got["posts"].([]any)
	if len(posts) != 1 {
		t.Fatalf("want 1 post in the feed, got %d", len(posts))
	}
	feedPost := posts[0].(map[string]any)
	if int64(feedPost["id"].(float64)) != postID {
		t.Fatalf("feed returned the wrong post: %v", feedPost)
	}
	if feedPost["comment_count"].(float64) != 2 {
		t.Fatalf("want comment_count 2, got %v", feedPost["comment_count"])
	}

	// Fetching the post returns the nested comment tree.
	status, got = do(t, ts, "GET", "/api/v1/posts/1", "", nil)
	if status != http.StatusOK {
		t.Fatalf("get post: want 200, got %d", status)
	}
	comments := got["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("want 1 top-level comment, got %d", len(comments))
	}
	replies := comments[0].(map[string]any)["replies"].([]any)
	if len(replies) != 1 {
		t.Fatalf("want 1 nested reply, got %d", len(replies))
	}
}
