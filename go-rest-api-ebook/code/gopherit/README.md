# gopherit — a Reddit-clone REST API in Go

This is the finished companion project for the ebook in
[`../../`](../../index.html) (“Go + REST — Build Your Own Reddit”).
Backend only — bring your own frontend.

## Run it

```bash
go run ./cmd/api
# in another terminal:
curl localhost:4000/api/v1/healthz
```

## Run the tests

```bash
go test ./...
```

## What's inside

| Path | What it is |
|---|---|
| `cmd/api/` | HTTP layer: main, routes, handlers, middleware, helpers |
| `internal/store/` | Data layer: SQLite schema + all queries |

Requires Go 1.24+. Dependencies: `modernc.org/sqlite` (pure-Go SQLite,
no C compiler needed) and `golang.org/x/crypto` (bcrypt).

## API at a glance

| Method & path | Auth | What it does |
|---|---|---|
| `GET  /api/v1/healthz` | – | liveness check |
| `POST /api/v1/users` | – | register |
| `POST /api/v1/tokens` | – | log in, get a bearer token |
| `GET  /api/v1/subreddits` | – | list communities |
| `POST /api/v1/subreddits` | ✔ | create a community |
| `GET  /api/v1/subreddits/{name}` | – | one community |
| `GET  /api/v1/subreddits/{name}/posts` | – | community feed (`?sort=hot|new|top&page=&page_size=`) |
| `GET  /api/v1/posts` | – | site-wide feed |
| `POST /api/v1/posts` | ✔ | create a text or link post |
| `GET  /api/v1/posts/{id}` | – | post + nested comment tree |
| `POST /api/v1/posts/{id}/comments` | ✔ | comment / reply (`parent_id`) |
| `POST /api/v1/posts/{id}/vote` | ✔ | vote `{"value": 1 | -1 | 0}` |
| `POST /api/v1/comments/{id}/vote` | ✔ | vote on a comment |

Send the token as `Authorization: Bearer <token>`.
