package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
)

// providerResolve resolves the provider+model from config, overridden by flags.
func providerResolve(root, provFlag, modelFlag string) (string, string) {
	pName, pModel := "ollama", ""
	if cfg, err := loadConfig(root); err == nil {
		pName, pModel = cfg.Provider, cfg.Model
	}
	if provFlag != "" {
		pName, pModel = provFlag, ""
	}
	if modelFlag != "" {
		pModel = modelFlag
	}
	return pName, pModel
}

// resolveDesignArg maps the positional arg to a design history file.
func resolveDesignArg(root, arg string) (string, error) {
	if arg == "" || arg == "latest" {
		recs := listDesigns(root, 100)
		if len(recs) == 0 {
			return "", errors.New("no designs found — run archi design first")
		}
		for _, r := range recs {
			if r.built {
				return filepath.Join(designsDir(root), r.stamp+"-"+r.slug+".md"), nil
			}
		}
		warn("no built design found — using the newest draft")
		return filepath.Join(designsDir(root), recs[0].stamp+"-"+recs[0].slug+".md"), nil
	}
	if _, err := os.Stat(arg); err == nil {
		return arg, nil
	}
	if p := filepath.Join(designsDir(root), arg); fileExists(p) {
		return p, nil
	}
	return "", fmt.Errorf("no such design: %s", arg)
}

// readDesignDoc parses a history file's front-matter and body.
func readDesignDoc(path string) (map[string]string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	s := string(data)
	fm := map[string]string{}
	if strings.HasPrefix(s, "---\n") {
		if end := strings.Index(s[4:], "\n---\n"); end >= 0 {
			for _, line := range strings.Split(s[4:4+end], "\n") {
				if i := strings.Index(line, ":"); i > 0 {
					fm[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
				}
			}
			return fm, s[4+end+5:], nil
		}
	}
	return fm, s, nil
}

// extractRepoPaths finds path-like tokens in doc that exist as files under root.
func extractRepoPaths(root, doc string, max int) []string {
	seen := map[string]bool{}
	var out []string
	fields := strings.FieldsFunc(doc, func(r rune) bool {
		return strings.ContainsRune(" \t\n`|()[],\"'", r)
	})
	for _, tok := range fields {
		tok = strings.Trim(tok, ".:;")
		if !strings.Contains(tok, "/") && !hasSourceExt(tok) {
			continue
		}
		rel, ok := safeRel(tok)
		if !ok || seen[rel] {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil || info.IsDir() {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
		if len(out) >= max {
			break
		}
	}
	return out
}

func hasSourceExt(s string) bool {
	for _, ext := range []string{".go", ".js", ".jsx", ".ts", ".tsx", ".py", ".rs", ".c", ".h", ".cpp", ".java", ".rb", ".php", ".sql", ".md", ".yaml", ".yml", ".toml", ".json"} {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}

// splitPlanAndFiles splits agent output at the first file/delete marker.
func splitPlanAndFiles(out string) (plan, blocks string) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "=== FILE:") || strings.HasPrefix(t, "=== DELETE:") {
			return strings.Join(lines[:i], "\n"), strings.Join(lines[i:], "\n")
		}
	}
	return out, ""
}

func cmdTestPlan(args []string) {
	fs := flag.NewFlagSet("test-plan", flag.ExitOnError)
	out := fs.String("o", "", "write the plan to a file instead of stdout")
	yes := fs.Bool("yes", false, "auto-approve writing test files")
	provider := fs.String("provider", "", "override provider")
	model := fs.String("model", "", "override model")
	temp := fs.Float64("temp", 0.2, "sampling temperature")
	maxTokens := fs.Int("max-tokens", 16000, "max output tokens")
	fs.Usage = testPlanUsage
	fs.Parse(args)

	root := "."
	if !isInitialized(root) {
		failf("no .archi cache here — run %s first", cyan("archi init"))
	}
	designPath, err := resolveDesignArg(root, fs.Arg(0))
	if err != nil {
		failf("%v", err)
	}
	fm, design, err := readDesignDoc(designPath)
	if err != nil {
		failf("%v", err)
	}

	pName, pModel := providerResolve(root, *provider, *model)
	mk := func(mt int) Provider { p, _ := newProvider(pName, pModel, *temp, mt); return p }

	profile, _ := os.ReadFile(profilePath(root))
	mapMD, _ := os.ReadFile(mapPath(root))
	touched := fitSeeds(readSeeds(root, extractRepoPaths(root, design, 20)), contextBudget())
	memory := fitMemory(loadMemory(root), memoryBudget())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	orch := newOrchestrator(root)
	banner()
	built := "draft"
	if fm["built"] == "yes" {
		built = "built"
	}
	info("Test plan for " + filepath.Base(designPath) + dim("  ·  "+built))

	sp := startSpinner("Writing tests")
	agent := Agent{Spec: AgentSpec{Role: RoleTestWriter, System: testWriterPrompt, MaxTokens: *maxTokens, Temp: *temp}, P: mk(*maxTokens)}
	in := TaskInput{Request: "write tests", Extra: map[string]string{
		"user_message": testPlanUserMessage(string(profile), design, string(mapMD), touched, memory)}}
	res := orch.runStream(ctx, agent, in, nil, sp)
	if res.Err != nil {
		sp.Abort()
		failf("%v", res.Err)
	}
	sp.Stop("Tests generated")

	plan, blocks := splitPlanAndFiles(res.Output)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(plan), 0o644); err != nil {
			failf("%v", err)
		}
		ok("Wrote " + *out)
	} else {
		fmt.Println(plan)
	}
	if _, _, err := applyChanges(root, parseFileBlocks(blocks), *yes); err != nil {
		failf("%v", err)
	}
}

func testPlanUsage() {
	fmt.Fprint(os.Stderr, `usage: archi test-plan [design-file|latest] [flags]

  Generate a test plan and test files for a saved design (default: newest built).

  -o string     write the plan to a file instead of stdout
  -yes          auto-approve writing test files
  -provider string / -model string   override the backend
`)
}
