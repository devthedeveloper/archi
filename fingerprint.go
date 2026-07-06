package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The repo fingerprint is a per-file hash of path + size + mtime, cached in
// .archi/fingerprint at init time. Recomputing it needs only a walk and a
// stat per file — no file reads — so `design` and `status` can cheaply tell
// whether the repo has drifted from the cached understanding, and
// `init -refresh` can update the profile from just the files that changed.

// hashFileMeta hashes a file's identity-relevant metadata deterministically.
// Any edit bumps mtime (and usually size), so the hash moves with the file.
func hashFileMeta(rel string, size int64, mtime time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", rel, size, mtime.UnixNano())))
	return hex.EncodeToString(sum[:])
}

// buildFingerprint stats each scanned file and returns rel path -> hash.
// Files that vanish between scan and stat are simply skipped.
func buildFingerprint(root string, files []fileInfo) map[string]string {
	fps := make(map[string]string, len(files))
	for _, f := range files {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(f.rel)))
		if err != nil {
			continue
		}
		fps[f.rel] = hashFileMeta(f.rel, info.Size(), info.ModTime())
	}
	return fps
}

// writeFingerprint persists the fingerprint as sorted "hash  path" lines —
// plain text, diffable, and order-independent to compare.
func writeFingerprint(root string, fps map[string]string) error {
	rels := make([]string, 0, len(fps))
	for rel := range fps {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	var b strings.Builder
	for _, rel := range rels {
		b.WriteString(fps[rel] + "  " + rel + "\n")
	}
	return os.WriteFile(fingerprintPath(root), []byte(b.String()), 0o644)
}

// loadFingerprint reads a stored fingerprint back into rel path -> hash.
func loadFingerprint(root string) (map[string]string, error) {
	data, err := os.ReadFile(fingerprintPath(root))
	if err != nil {
		return nil, err
	}
	fps := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		hash, rel, found := strings.Cut(line, "  ")
		if !found || hash == "" || rel == "" {
			continue
		}
		fps[rel] = hash
	}
	return fps, nil
}

// fingerprintDiff is the drift between a stored fingerprint and the repo now.
type fingerprintDiff struct {
	added    []string
	modified []string
	removed  []string
}

func (d fingerprintDiff) empty() bool {
	return len(d.added) == 0 && len(d.modified) == 0 && len(d.removed) == 0
}

// summary renders the drift for humans: "12 files added/modified, 3 removed".
func (d fingerprintDiff) summary() string {
	var parts []string
	if n := len(d.added) + len(d.modified); n > 0 {
		noun := "files"
		if n == 1 {
			noun = "file"
		}
		parts = append(parts, fmt.Sprintf("%d %s added/modified", n, noun))
	}
	if n := len(d.removed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", n))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

// diffFingerprints compares a stored fingerprint against the current one.
// Results are sorted so output (and tests) are deterministic.
func diffFingerprints(old, cur map[string]string) fingerprintDiff {
	var d fingerprintDiff
	for rel, h := range cur {
		switch oh, ok := old[rel]; {
		case !ok:
			d.added = append(d.added, rel)
		case oh != h:
			d.modified = append(d.modified, rel)
		}
	}
	for rel := range old {
		if _, ok := cur[rel]; !ok {
			d.removed = append(d.removed, rel)
		}
	}
	sort.Strings(d.added)
	sort.Strings(d.modified)
	sort.Strings(d.removed)
	return d
}

// needsFullRebuild reports whether drift is too large for an incremental
// refresh — more than 40% of the previously fingerprinted files were touched.
// With no previous files to compare against, only a full rebuild makes sense.
func needsFullRebuild(d fingerprintDiff, oldCount int) bool {
	if oldCount == 0 {
		return true
	}
	touched := len(d.added) + len(d.modified) + len(d.removed)
	return touched*100 > oldCount*40
}

// changedFiles filters scan results down to the files a diff marks as added
// or modified, preserving scan order — the inputs an incremental profile
// refresh needs to read.
func changedFiles(files []fileInfo, d fingerprintDiff) []fileInfo {
	want := make(map[string]bool, len(d.added)+len(d.modified))
	for _, rel := range d.added {
		want[rel] = true
	}
	for _, rel := range d.modified {
		want[rel] = true
	}
	var out []fileInfo
	for _, f := range files {
		if want[f.rel] {
			out = append(out, f)
		}
	}
	return out
}

// checkFreshness recomputes the fingerprint (stat only, no file reads) and
// diffs it against the stored one. ok is false when there is no stored
// fingerprint to compare against (a cache from an older archi) or the repo
// can't be walked — callers should stay quiet rather than guess.
func checkFreshness(root string) (d fingerprintDiff, ok bool) {
	old, err := loadFingerprint(root)
	if err != nil {
		return fingerprintDiff{}, false
	}
	files, err := scanRepo(root)
	if err != nil {
		return fingerprintDiff{}, false
	}
	return diffFingerprints(old, buildFingerprint(root, files)), true
}

// cacheAge reports how old the cache is, from the fingerprint's mtime (it is
// rewritten on every init and refresh), falling back to the profile's. Zero
// means unknown.
func cacheAge(root string) time.Duration {
	for _, p := range []string{fingerprintPath(root), profilePath(root)} {
		if fi, err := os.Stat(p); err == nil {
			return time.Since(fi.ModTime())
		}
	}
	return 0
}

// humanAge formats a duration as a coarse age: "3d", "5h", "12m", "moments".
func humanAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return "moments"
	}
}
