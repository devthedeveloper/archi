package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// toolchain is a detected verification recipe.
type toolchain struct {
	name string     // "go", "npm", "cargo", "make"
	cmds [][]string // argv lists, run in order, stop at first failure
}

// npmDefaultTest is the placeholder script `npm init` writes; it is not a real test.
const npmDefaultTest = `echo "Error: no test specified" && exit 1`

// detectToolchain probes root in fixed order and returns the FIRST match.
func detectToolchain(root string) (toolchain, bool) {
	if fileExists(filepath.Join(root, "go.mod")) {
		return toolchain{name: "go", cmds: [][]string{{"go", "build", "./..."}, {"go", "vet", "./..."}}}, true
	}
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil && hasTestScript(data) {
		return toolchain{name: "npm", cmds: [][]string{{"npm", "test", "--silent"}}}, true
	}
	if fileExists(filepath.Join(root, "Cargo.toml")) {
		return toolchain{name: "cargo", cmds: [][]string{{"cargo", "check"}}}, true
	}
	if data, err := os.ReadFile(filepath.Join(root, "Makefile")); err == nil && makefileHasTest(data) {
		return toolchain{name: "make", cmds: [][]string{{"make", "test"}}}, true
	}
	return toolchain{}, false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// hasTestScript reports whether package.json has a real scripts.test (present
// and not npm's default placeholder).
func hasTestScript(pkgJSON []byte) bool {
	var m struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(pkgJSON, &m) != nil {
		return false
	}
	t, ok := m.Scripts["test"]
	return ok && strings.TrimSpace(t) != "" && strings.TrimSpace(t) != npmDefaultTest
}

// makefileHasTest reports whether a line matching ^test[:\s] starts at column 0.
func makefileHasTest(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "test") && len(line) > 4 {
			switch line[4] {
			case ':', ' ', '\t':
				return true
			}
		}
	}
	return false
}

// verifyResult is one verification pass.
type verifyResult struct {
	ok     bool
	cmd    string // the argv that failed (or last run), joined by spaces
	output string // combined stdout+stderr, truncated
}

// defaultVerifyTimeout caps each verification command.
const defaultVerifyTimeout = 3 * time.Minute

// verifyTimeout resolves ARCHI_VERIFY_TIMEOUT (a Go duration), else the default.
func verifyTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ARCHI_VERIFY_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		warn("ignoring invalid ARCHI_VERIFY_TIMEOUT=" + v)
	}
	return defaultVerifyTimeout
}

const (
	maxVerifyHead = 80
	maxVerifyTail = 40
)

// runVerify runs tc.cmds in order, each with its own timeout, stopping at the
// first failure. Output is truncated for display.
func runVerify(ctx context.Context, root string, tc toolchain) verifyResult {
	for _, argv := range tc.cmds {
		cctx, cancel := context.WithTimeout(ctx, verifyTimeout())
		cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		timedOut := cctx.Err() == context.DeadlineExceeded
		cancel()
		if timedOut {
			return verifyResult{cmd: strings.Join(argv, " "),
				output: truncateOutput(string(out), maxVerifyHead, maxVerifyTail) + "\ntimed out after " + verifyTimeout().String()}
		}
		if err != nil {
			return verifyResult{cmd: strings.Join(argv, " "), output: truncateOutput(string(out), maxVerifyHead, maxVerifyTail)}
		}
	}
	return verifyResult{ok: true}
}

// truncateOutput keeps the first head and last tail lines when the text exceeds
// head+tail lines, joining with a "… N lines omitted …" marker.
func truncateOutput(s string, head, tail int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= head+tail {
		return s
	}
	kept := append([]string{}, lines[:head]...)
	kept = append(kept, fmt.Sprintf("… %d lines omitted …", len(lines)-head-tail))
	kept = append(kept, lines[len(lines)-tail:]...)
	return strings.Join(kept, "\n")
}

// errFileRe matches source file paths (optionally with :line) in tool output.
var errFileRe = regexp.MustCompile(`([A-Za-z0-9_][A-Za-z0-9_./\-]*\.(?:go|js|jsx|ts|tsx|mjs|rs|py|c|h|cpp|java))(?::\d+)?`)

// mapErrorsToFiles extracts real, in-repo source paths from tool output,
// deduplicated in first-seen order, capped at 8.
func mapErrorsToFiles(output, root string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range errFileRe.FindAllStringSubmatch(output, -1) {
		rel, ok := safeRel(strings.TrimPrefix(m[1], "./"))
		if !ok || seen[rel] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// toolchainCmds renders a toolchain's commands for display.
func toolchainCmds(tc toolchain) string {
	parts := make([]string, len(tc.cmds))
	for i, c := range tc.cmds {
		parts[i] = strings.Join(c, " ")
	}
	return strings.Join(parts, " , ")
}

// repairLoop verifies after an approved apply and repairs failures up to
// s.maxRepair times, each repair previewed/confirmed like any apply. Returns
// whether verification finally passed and the backup stamps repair created
// (newest last), for the rollback hint.
func (s *session) repairLoop(ctx context.Context, o *Orchestrator, design, firstStamp string) (bool, []string) {
	tc, has := detectToolchain(s.root)
	if !has {
		warn("no toolchain detected — skipping verification")
		return true, nil
	}
	info("Verifying: " + toolchainCmds(tc))
	vr := runVerify(ctx, s.root, tc)
	if vr.ok {
		ok("verified: " + tc.name)
		return true, nil
	}

	var stamps []string
	for i := 1; i <= s.maxRepair; i++ {
		if ctx.Err() != nil {
			return false, stamps
		}
		files := mapErrorsToFiles(vr.output, s.root)
		if len(files) == 0 {
			break
		}
		warn(vr.cmd + " failed — attempting repair " + fmt.Sprintf("%d/%d", i, s.maxRepair))
		seeds := fitSeeds(readSeeds(s.root, files), contextBudget())
		agent := Agent{Spec: AgentSpec{Role: RoleRepairer, System: repairPrompt, MaxTokens: 8000, Temp: 0}, P: s.builder()}
		in := TaskInput{Request: "fix the failure", Extra: map[string]string{
			"user_message": repairUserMessage(firstN(design, 1500), vr.cmd, vr.output, seeds)}}
		res := o.Run(ctx, agent, in)
		if res.Err != nil {
			break
		}
		changes := parseFileBlocks(res.Output)
		if len(changes) == 0 {
			break
		}
		applied, stamp, err := applyChanges(s.root, changes, s.yes)
		if err != nil || !applied {
			break
		}
		if stamp != "" {
			stamps = append(stamps, stamp)
		}
		info("Verifying: " + toolchainCmds(tc))
		vr = runVerify(ctx, s.root, tc)
		if vr.ok {
			ok(fmt.Sprintf("verified after %d repair(s)", i))
			return true, stamps
		}
	}

	warn(fmt.Sprintf("still failing after %d repair attempt(s): %s", len(stamps), vr.cmd))
	printTail(vr.output, 15)
	all := append([]string{}, stamps...)
	all = append(all, firstStamp)
	reverseStrings(all)
	hint := "undo everything:  " + cyan("archi rollback -run "+all[0])
	if len(all) > 1 {
		hint += dim("  (repeat for: " + strings.Join(all[1:], ", ") + " — newest first)")
	}
	info(hint)
	return false, stamps
}

// printTail prints the last n lines of output to stderr, dimmed.
func printTail(output string, n int) {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		fmt.Fprintln(os.Stderr, "      "+dim(l))
	}
}

func reverseStrings(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// firstN returns the first n bytes of s, backed off to a UTF-8 boundary.
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
