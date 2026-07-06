package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// packet is one unit of parallel work produced by the planner.
type packet struct {
	ID        string // lowercase slug, unique in plan
	Title     string
	Files     []string // relative paths it creates/modifies; validated by safeRel
	DependsOn []string // packet IDs
	Notes     string
}

// maxPackets caps how many packets a plan may contain.
const maxPackets = 8

// minSwarmFiles is the threshold below which the build stays single-shot.
const minSwarmFiles = 4

// parsePackets extracts the LAST ```packets fenced block and parses it. Pure.
// Errors on: no block; an entry with an empty id or no files; a duplicate id.
func parsePackets(s string) ([]packet, error) {
	block, ok := lastFencedBlock(s, "packets")
	if !ok {
		return nil, errors.New("no packets block found")
	}
	var packets []packet
	var cur *packet
	seen := map[string]bool{}
	for _, line := range strings.Split(block, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "- id:"):
			id := strings.TrimSpace(strings.TrimPrefix(t, "- id:"))
			if id == "" {
				return nil, errors.New("packet with an empty id")
			}
			if seen[id] {
				return nil, fmt.Errorf("duplicate packet id: %s", id)
			}
			seen[id] = true
			packets = append(packets, packet{ID: id})
			cur = &packets[len(packets)-1]
		case cur == nil:
			// ignore anything before the first packet
		case strings.HasPrefix(t, "title:"):
			cur.Title = strings.TrimSpace(strings.TrimPrefix(t, "title:"))
		case strings.HasPrefix(t, "files:"):
			cur.Files = splitCommaList(strings.TrimPrefix(t, "files:"))
		case strings.HasPrefix(t, "dependsOn:"):
			cur.DependsOn = splitCommaList(strings.TrimPrefix(t, "dependsOn:"))
		case strings.HasPrefix(t, "notes:"):
			cur.Notes = strings.TrimSpace(strings.TrimPrefix(t, "notes:"))
		}
	}
	if len(packets) == 0 {
		return nil, errors.New("no packets parsed")
	}
	for _, p := range packets {
		if len(p.Files) == 0 {
			return nil, fmt.Errorf("packet %s lists no files", p.ID)
		}
	}
	return packets, nil
}

// splitCommaList splits on "," and trims, dropping empties.
func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validatePackets checks ids are unique and non-empty, every file passes
// safeRel, no two packets share a file, every dependsOn names a known id, and
// the plan is within maxPackets. Returns the first error found.
func validatePackets(ps []packet) error {
	if len(ps) > maxPackets {
		return fmt.Errorf("too many packets: %d (max %d)", len(ps), maxPackets)
	}
	ids := map[string]bool{}
	owner := map[string]string{}
	for _, p := range ps {
		if p.ID == "" {
			return errors.New("packet with an empty id")
		}
		if ids[p.ID] {
			return fmt.Errorf("duplicate packet id: %s", p.ID)
		}
		ids[p.ID] = true
		for _, f := range p.Files {
			if _, ok := safeRel(f); !ok {
				return fmt.Errorf("packet %s has an unsafe path: %s", p.ID, f)
			}
			if prev, dup := owner[f]; dup {
				return fmt.Errorf("file %s is claimed by both %s and %s", f, prev, p.ID)
			}
			owner[f] = p.ID
		}
	}
	for _, p := range ps {
		for _, d := range p.DependsOn {
			if !ids[d] {
				return fmt.Errorf("packet %s depends on unknown id: %s", p.ID, d)
			}
		}
	}
	return nil
}

// topoLevels orders packets into dependency levels via Kahn's algorithm: each
// level is the set of packets whose dependencies are all satisfied, sorted by
// id. A cycle returns an error naming the packets stuck in it.
func topoLevels(ps []packet) ([][]string, error) {
	indeg := make(map[string]int, len(ps))
	dependents := map[string][]string{}
	for _, p := range ps {
		indeg[p.ID] = 0
	}
	for _, p := range ps {
		for _, d := range p.DependsOn {
			indeg[p.ID]++
			dependents[d] = append(dependents[d], p.ID)
		}
	}
	emitted := map[string]bool{}
	var levels [][]string
	for len(emitted) < len(ps) {
		var level []string
		for _, p := range ps {
			if !emitted[p.ID] && indeg[p.ID] == 0 {
				level = append(level, p.ID)
			}
		}
		if len(level) == 0 {
			var remaining []string
			for _, p := range ps {
				if !emitted[p.ID] {
					remaining = append(remaining, p.ID)
				}
			}
			sort.Strings(remaining)
			return nil, fmt.Errorf("dependency cycle among packets: %s", strings.Join(remaining, ", "))
		}
		sort.Strings(level)
		for _, id := range level {
			emitted[id] = true
			for _, dep := range dependents[id] {
				indeg[dep]--
			}
		}
		levels = append(levels, level)
	}
	return levels, nil
}

// countDesignPaths counts distinct safeRel-valid, path-like first cells in the
// design's "## Changes by area" table. A missing section yields 0.
func countDesignPaths(design string) int {
	inSection := false
	seen := map[string]bool{}
	for _, line := range strings.Split(design, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			inSection = strings.Contains(strings.ToLower(t), "changes by area")
			continue
		}
		if !inSection || !strings.HasPrefix(t, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(t, "|"), "|")
		if len(cells) == 0 {
			continue
		}
		tok := strings.Trim(strings.TrimSpace(cells[0]), "`")
		if !strings.ContainsAny(tok, "/.") {
			continue
		}
		if _, ok := safeRel(tok); ok {
			seen[tok] = true
		}
	}
	return len(seen)
}

// shouldSwarm reports whether a design is big enough to split into packets.
func shouldSwarm(design string) bool { return countDesignPaths(design) >= minSwarmFiles }
