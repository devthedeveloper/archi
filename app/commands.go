package main

import (
	"fmt"
	"strings"
)

// command is one parsed /archi invocation.
type command struct {
	Name     string // "design" | "build" | "review" | "help"
	PR       bool   // design --pr
	Request  string // rest of comment for design; "" otherwise
	helpNote string // extra line above helpComment explaining the problem
}

// maxRequestBytes bounds an accepted comment; larger requests are refused at
// parse time, before anything is cloned or sent to a model.
const maxRequestBytes = 64 << 10

// parseCommand extracts a command from a comment body. The FIRST non-blank
// line must start with "/archi" — anything else is not our comment and
// returns (nil, false) so the bot stays silent. A bare "/archi", an unknown
// subcommand, or an invalid request returns {Name:"help"} — the user clearly
// addressed the bot, so it replies with usage instead of silence. Pure.
func parseCommand(body string) (*command, bool) {
	all := strings.Split(body, "\n")
	idx := 0
	for idx < len(all) && strings.TrimSpace(all[idx]) == "" {
		idx++
	}
	if idx == len(all) {
		return nil, false
	}
	first := strings.TrimSpace(all[idx])
	after, isCmd := strings.CutPrefix(first, "/archi")
	if !isCmd || (after != "" && after[0] != ' ' && after[0] != '\t') {
		return nil, false // not addressed to us (e.g. "/archive")
	}
	if len(body) > maxRequestBytes {
		return &command{Name: "help", helpNote: "that request is too large (max 64 KiB)"}, true
	}
	if strings.ContainsRune(body, 0) {
		return &command{Name: "help", helpNote: "the request contains a NUL byte"}, true
	}

	pr := false
	var tokens []string
	for _, t := range strings.Fields(after) {
		if t == "--pr" {
			pr = true
			continue
		}
		tokens = append(tokens, t)
	}
	if len(tokens) == 0 {
		return &command{Name: "help"}, true
	}
	tail := strings.Join(all[idx+1:], "\n")
	switch sub := tokens[0]; sub {
	case "design":
		req := strings.TrimSpace(strings.Join(tokens[1:], " ") + "\n" + tail)
		req = unwrapQuotes(req)
		if req == "" {
			return &command{Name: "help", PR: pr,
				helpNote: "design needs a request — try `/archi design \"add rate limiting per API key\"`"}, true
		}
		return &command{Name: "design", PR: pr, Request: req}, true
	case "build":
		return &command{Name: "build"}, true
	case "review":
		return &command{Name: "review"}, true
	case "help":
		return &command{Name: "help"}, true
	default:
		return &command{Name: "help", helpNote: fmt.Sprintf("unknown subcommand %q", sub)}, true
	}
}

// unwrapQuotes strips one pair of matching quotes fully wrapping s, so
// `/archi design "add webhooks"` works as documented. A quote character that
// also appears inside stays untouched — the wrap must be unambiguous.
func unwrapQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if (q != '"' && q != '\'' && q != '`') || s[len(s)-1] != q {
		return s
	}
	inner := s[1 : len(s)-1]
	if strings.IndexByte(inner, q) >= 0 {
		return s
	}
	return strings.TrimSpace(inner)
}

// helpComment is the usage text posted whenever the bot is addressed but the
// command can't run as written.
const helpComment = `**archi commands** (you need write access to the repo):

- ` + "`/archi design <request>`" + ` — design a change against this codebase; the design is posted as a comment
- ` + "`/archi design --pr <request>`" + ` — design and open a draft PR containing the design doc; review it, then comment ` + "`/archi build`" + ` on that PR
- ` + "`/archi build`" + ` — on an archi design PR: generate the code from the approved design and push it to the PR branch
- ` + "`/archi review`" + ` — review this PR's diff with archi's critic panel (also runs automatically when ` + "`auto_review: true`" + `)

Configure per repo in ` + "`.github/archi.yml`" + ` (flat ` + "`key: value`" + ` lines): ` + "`provider`, `model`, `critic_model`, `agents` (full|fast), `auto_review` (true|false), `max_tokens`" + `.

Docs: https://github.com/devthedeveloper/archi`
