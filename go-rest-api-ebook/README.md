# Go + REST — Build Your Own Reddit 🐹

A colorful, handwritten-notes-style ebook that teaches building REST APIs in
Go by writing the complete backend of a Reddit clone (**gopherit**) —
backend only, so any frontend can plug in.

## Read it

Open [`index.html`](index.html) in a browser — that's the cover and table of
contents. Everything is self-contained (fonts included), works offline, and
prints nicely if you want a PDF (`Ctrl+P`).

## Chapters

1. **The Game Plan** — HTTP anatomy, REST, the architecture map
2. **Hello, HTTP** — first server, Go 1.22+ routing, JSON out
3. **A Real Project Skeleton** — cmd/internal, app struct, flags, slog
4. **Speaking JSON** — readJSON/writeJSON, envelopes, error helpers, validation
5. **A Database in a File** — SQLite (pure Go), schema, the store layer
6. **Users & Passwords** — bcrypt, `json:"-"`, register endpoint
7. **Login & Tokens** — stateful bearer tokens, hashed at rest
8. **Middleware** — panic recovery, logging, CORS, auth via context
9. **Subreddits** — first full resource
10. **Posts & Feeds** — pagination, sorting, the hot formula
11. **Comments & Votes** — nested trees, upserts, transactions
12. **Polish, Test, Ship** — graceful shutdown, httptest, deploy

## The code

- [`code/gopherit/`](code/gopherit/) — the finished project. Compiles with
  Go 1.24+, passes `go test ./...`. Every code block in the book is injected
  from these files, so book and code can never drift apart.
- [`code/snippets/`](code/snippets/) — the small standalone programs from
  chapters 2–3, also compiled and tested.

Companion to the [gocheatsheet](https://github.com/devthedeveloper/gocheatsheet).
