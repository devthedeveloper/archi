package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	provider := fs.String("provider", "ollama", "LLM provider: ollama or anthropic")
	model := fs.String("model", "", "model id (default: provider default)")
	force := fs.Bool("force", false, "rebuild even if .archi already exists")
	maxFileKB := fs.Int64("max-file-kb", 256, "skip files larger than this when sampling")
	fs.Usage = func() { initUsage() }
	fs.Parse(args)

	root := "."
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		failf("%s is not a directory", root)
	}
	if isInitialized(root) && !*force {
		failf(".archi already exists in %s — pass -force to rebuild", root)
	}

	prov, err := newProvider(*provider, *model, 0.3, 4000)
	if err != nil {
		failf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	banner()

	// 1. Scan.
	files, err := scanRepo(root)
	if err != nil {
		failf("scan failed: %v", err)
	}
	if len(files) == 0 {
		failf("no files found under %s", root)
	}
	stats, totalFiles, totalTokens := languageStats(files)
	ok("Scanned " + humanCount(totalFiles) + " files · ~" + humanCount(totalTokens) + " tokens")
	langBars(stats, totalTokens, true)

	// 2. Write the deterministic map.
	if err := os.MkdirAll(archiDir(root), 0o755); err != nil {
		failf("%v", err)
	}
	mapMD := buildMap(root, files)
	if err := os.WriteFile(mapPath(root), []byte(mapMD), 0o644); err != nil {
		failf("%v", err)
	}
	ok("Mapped repo → " + mapPath(root))

	// 3. Select the profile seed and ask the model to understand the repo.
	seed, seedTokens := selectSeed(root, files, 40000, *maxFileKB*1024)
	info("Reading " + humanCount(len(seed)) + " key files (~" + humanCount(seedTokens) + " tokens) with " + prov.Name())

	sp := startSpinner("Understanding your codebase")
	profile, err := runModel(ctx, prov, analystPrompt, analystUserMessage(mapMD, seed), nil, sp)
	if err != nil {
		sp.Abort()
		failf("%v", err)
	}
	sp.Stop("Understood → " + profilePath(root))
	if err := os.WriteFile(profilePath(root), []byte(profile), 0o644); err != nil {
		failf("%v", err)
	}

	// 4. Persist config and tidy .gitignore.
	langMap := map[string]int{}
	for _, s := range stats {
		langMap[s.name] = s.tokens
	}
	cfg := &Config{
		Version:       version,
		Provider:      *provider,
		Model:         prov.Model(),
		Root:          root,
		InitializedAt: time.Now().UTC().Format(time.RFC3339),
		Languages:     langMap,
		FileCount:     totalFiles,
		ApproxTokens:  totalTokens,
	}
	if err := cfg.save(root); err != nil {
		failf("%v", err)
	}
	ensureGitignored(root)

	os.Stderr.WriteString("\n")
	ok(bold("Ready.") + "  Now run  " + cyan(`archi design "add …"`))
	os.Stderr.WriteString("\n")
}

// selectSeed picks a token-bounded, priority-ordered subset of files for the
// analyst: manifests and docs first, then entry points and config, then a
// representative sample of source until the budget fills.
func selectSeed(root string, files []fileInfo, budget int, maxFileBytes int64) ([]seedFile, int) {
	var picked []seedFile
	seen := map[string]bool{}
	used := 0

	add := func(f fileInfo) {
		if seen[f.rel] {
			return
		}
		data, ok := readTextFile(root, f.rel, maxFileBytes)
		if !ok {
			return
		}
		t := estimateTokens(string(data))
		if used+t > budget {
			return
		}
		seen[f.rel] = true
		used += t
		picked = append(picked, seedFile{rel: f.rel, lang: f.lang, content: string(data)})
	}

	for _, f := range files {
		if isManifest(f.rel) {
			add(f)
		}
	}
	for _, f := range files {
		if isReadmeOrDoc(f.rel) {
			add(f)
		}
	}
	for _, f := range files {
		if isEntryPoint(f.rel) {
			add(f)
		}
	}
	for _, f := range files {
		if isConfigFile(f.rel) {
			add(f)
		}
	}

	// Remaining source, shallowest paths first (top-level code carries the most signal).
	rest := make([]fileInfo, len(files))
	copy(rest, files)
	sort.SliceStable(rest, func(i, j int) bool {
		di, dj := strings.Count(rest[i].rel, "/"), strings.Count(rest[j].rel, "/")
		if di != dj {
			return di < dj
		}
		return rest[i].rel < rest[j].rel
	})
	for _, f := range rest {
		if f.lang != "" {
			add(f)
		}
	}
	return picked, used
}

func base(rel string) string { return strings.ToLower(filepath.Base(rel)) }

func isManifest(rel string) bool {
	switch base(rel) {
	case "go.mod", "package.json", "pyproject.toml", "requirements.txt", "cargo.toml",
		"pom.xml", "gemfile", "composer.json", "dockerfile", "makefile", "build.gradle":
		return true
	}
	b := base(rel)
	return strings.HasPrefix(b, "docker-compose") || strings.HasSuffix(b, ".tf")
}

func isReadmeOrDoc(rel string) bool {
	return strings.HasPrefix(base(rel), "readme") || strings.HasPrefix(rel, "docs/")
}

func isEntryPoint(rel string) bool {
	b := base(rel)
	for _, p := range []string{"main.", "index.", "app.", "server."} {
		if strings.HasPrefix(b, p) {
			return true
		}
	}
	return strings.HasPrefix(rel, "cmd/")
}

func isConfigFile(rel string) bool {
	b := base(rel)
	if b == ".env.example" || strings.HasPrefix(b, "settings.") || strings.Contains(b, ".config.") {
		return true
	}
	return strings.HasPrefix(rel, "config/")
}

// ensureGitignored appends .archi/ to an existing .gitignore if it's missing.
// It never creates a .gitignore where none exists.
func ensureGitignored(root string) {
	gi := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(gi)
	if err != nil {
		return
	}
	if strings.Contains(string(data), ".archi") {
		return
	}
	f, err := os.OpenFile(gi, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	prefix := "\n"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n\n"
	}
	if _, err := f.WriteString(prefix + "# archi cache\n.archi/\n"); err == nil {
		info("Added .archi/ to .gitignore")
	}
}
