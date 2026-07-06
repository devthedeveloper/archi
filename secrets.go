package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

// redactedMark replaces every likely secret before content reaches a prompt.
const redactedMark = "[REDACTED]"

// secretRule pairs a compiled pattern with the submatch to redact: group 0
// replaces the whole match (self-identifying tokens), a higher group replaces
// only the value so surrounding code (key names, quotes) stays readable.
type secretRule struct {
	re    *regexp.Regexp
	group int
}

// secretRules is compiled once at package init. Ordered most-specific first so
// a token redacted by a narrow rule can't be double-counted by the generic one.
// Every value requires a long-enough body to keep false positives low.
var secretRules = []secretRule{
	// Private key blocks (RSA, EC, OPENSSH, PKCS#8, ...), including the armor.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), 0},
	// AWS access key ids are self-identifying.
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), 0},
	// AWS secret keys only in assignments — the value alone is just base64-ish.
	{regexp.MustCompile(`(?i)aws[_-]?secret[_-]?(?:access[_-]?)?key\s*(?::=|[:=])\s*["']?([A-Za-z0-9/+=]{20,})`), 1},
	// GitHub tokens (classic and fine-grained).
	{regexp.MustCompile(`\b(?:ghp|gho|ghs|ghu|ghr)_[A-Za-z0-9]{20,}\b`), 0},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), 0},
	// Slack tokens.
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), 0},
	// Stripe live keys.
	{regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{16,}\b`), 0},
	// Bearer tokens in headers; redact only the credential.
	{regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9+/_.=-]{16,})`), 1},
	// Generic credential assignments (key: value, key = value, key := value);
	// the 16+ char value requirement is what keeps ordinary code alive.
	{regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|password|passwd)\s*(?::=|[:=])\s*["']?([A-Za-z0-9+/_.-]{16,})`), 1},
}

// redactSecrets replaces likely secrets in content with redactedMark and
// returns the cleaned text plus how many replacements were made.
func redactSecrets(content string) (string, int) {
	count := 0
	for _, rule := range secretRules {
		matches := rule.re.FindAllStringSubmatchIndex(content, -1)
		if len(matches) == 0 {
			continue
		}
		var b strings.Builder
		last := 0
		for _, m := range matches {
			start, end := m[2*rule.group], m[2*rule.group+1]
			if start < 0 {
				continue
			}
			b.WriteString(content[last:start])
			b.WriteString(redactedMark)
			last = end
			count++
		}
		b.WriteString(content[last:])
		content = b.String()
	}
	return content, count
}

// redactForPrompt is the choke point for file contents entering an LLM prompt:
// it redacts likely secrets and warns on stderr when anything was removed.
func redactForPrompt(rel, content string) string {
	clean, n := redactSecrets(content)
	if n > 0 {
		noun := "secrets"
		if n == 1 {
			noun = "secret"
		}
		warn("redacted " + humanCount(n) + " likely " + noun + " from " + rel)
	}
	return clean
}

// isSecretFile reports whether a path names a file that likely holds
// credentials wholesale (.env, .env.*, *.pem, *.key, id_rsa*). Such files are
// excluded from scanning entirely rather than redacted line by line.
func isSecretFile(rel string) bool {
	b := strings.ToLower(filepath.Base(rel))
	switch {
	case b == ".env" || strings.HasPrefix(b, ".env."):
		return true
	case strings.HasSuffix(b, ".pem") || strings.HasSuffix(b, ".key"):
		return true
	case strings.HasPrefix(b, "id_rsa"):
		return true
	}
	return false
}
