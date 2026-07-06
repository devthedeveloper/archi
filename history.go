package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Design history keeps every produced design in .archi/designs/ as
// <timestamp>-<slug>.md with a small front-matter header, so past decisions
// stay on disk and greppable. History is advisory — callers must warn and
// continue when it fails, never abort a design run.

// stampLayout names timestamped artifacts (design history entries, backup
// runs), e.g. 20260706-153012. Lexical order is chronological order.
const stampLayout = "20060102-150405"

// maxSlugLen caps the request-derived part of a history filename.
const maxSlugLen = 40

// designsDir returns the design history directory for root.
func designsDir(root string) string { return filepath.Join(archiDir(root), "designs") }

// designSlug derives a filename slug from the first ~40 chars of a request:
// lowercased, with runs of anything outside [a-z0-9] collapsed to single
// dashes. An empty result falls back to "design".
func designSlug(request string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(request) {
		if b.Len() >= maxSlugLen {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "design"
	}
	return s
}

// designHistoryPath names the history file for a request designed at t.
func designHistoryPath(root, request string, t time.Time) string {
	return filepath.Join(designsDir(root), t.Format(stampLayout)+"-"+designSlug(request)+".md")
}

// writeDesignHistory writes (or rewrites) path as a front-matter header
// followed by the design document.
func writeDesignHistory(path, request, provider string, t time.Time, built bool, doc string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	req := strings.Join(strings.Fields(request), " ")
	if len(req) > 200 {
		req = req[:200] + "…"
	}
	flag := "no"
	if built {
		flag = "yes"
	}
	header := fmt.Sprintf("---\nrequest: %s\nprovider: %s\ndate: %s\nbuilt: %s\n---\n\n",
		req, provider, t.Format(time.RFC3339), flag)
	return os.WriteFile(path, []byte(header+ensureTrailingNewline(doc)), 0o644)
}

// markDesignBuilt flips a saved design's header from "built: no" to
// "built: yes".
func markDesignBuilt(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out := strings.Replace(string(data), "\nbuilt: no\n", "\nbuilt: yes\n", 1)
	return os.WriteFile(path, []byte(out), 0o644)
}

// designRecord is one entry in the design history listing.
type designRecord struct {
	stamp string
	slug  string
	built bool
}

// listDesigns returns up to n design records, newest first. A missing or
// unreadable history directory reads as empty.
func listDesigns(root string, n int) []designRecord {
	entries, err := os.ReadDir(designsDir(root))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) > n {
		names = names[:n]
	}
	out := make([]designRecord, 0, len(names))
	for _, name := range names {
		r := parseDesignName(name)
		if data, err := os.ReadFile(filepath.Join(designsDir(root), name)); err == nil {
			head := string(data)
			if len(head) > 512 {
				head = head[:512]
			}
			r.built = strings.Contains(head, "\nbuilt: yes\n")
		}
		out = append(out, r)
	}
	return out
}

// parseDesignName splits a history filename into its stamp and slug parts.
func parseDesignName(name string) designRecord {
	base := strings.TrimSuffix(name, ".md")
	if len(base) > len(stampLayout) && base[len(stampLayout)] == '-' {
		return designRecord{stamp: base[:len(stampLayout)], slug: base[len(stampLayout)+1:]}
	}
	return designRecord{stamp: base}
}
