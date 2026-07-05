package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
)

// stringList collects a repeatable flag (e.g. -focus a -focus b).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// providerFactory builds a provider at a chosen output-token budget. The
// interview and design fit in a normal budget; code generation needs more room.
type providerFactory func(maxTokens int) Provider

func cmdDesign(args []string) {
	fs := flag.NewFlagSet("design", flag.ExitOnError)
	var focus stringList
	fs.Var(&focus, "focus", "glob of files to include as extra grounding (repeatable)")
	file := fs.String("f", "", "read the request from this file")
	out := fs.String("o", "", "write the design to this file instead of stdout")
	htmlFile := fs.String("html", "", "also render the design to a standalone HTML file (drawn diagrams)")
	provider := fs.String("provider", "", "override provider (default: from .archi)")
	model := fs.String("model", "", "override model (default: from .archi)")
	temp := fs.Float64("temp", 0.4, "sampling temperature")
	maxTokens := fs.Int("max-tokens", 8000, "max output tokens for the design")
	noInteractive := fs.Bool("no-interactive", false, "skip the interview and review loop")
	build := fs.Bool("build", false, "after designing, generate the code (non-interactive)")
	yes := fs.Bool("yes", false, "auto-approve writing generated files")
	noStream := fs.Bool("no-stream", false, "wait for the full response instead of streaming")
	fs.Usage = func() { designUsage() }
	fs.Parse(args)

	root := "."
	if !isInitialized(root) {
		failf("no .archi cache here — run %s first", cyan("archi init"))
	}
	cfg, err := loadConfig(root)
	if err != nil {
		failf("reading .archi: %v", err)
	}

	var fileData []byte
	if *file != "" {
		if fileData, err = os.ReadFile(*file); err != nil {
			failf("%v", err)
		}
	}
	request, err := resolveInput(fs.Args(), fileData, *file != "", os.Stdin, isTTY(os.Stdin))
	if err != nil {
		failf("%v", err)
	}

	// Resolve the backend; flags override what init stored.
	pName, pModel := cfg.Provider, cfg.Model
	if *provider != "" {
		pName, pModel = *provider, ""
	}
	if *model != "" {
		pModel = *model
	}
	if _, err := newProvider(pName, pModel, *temp, *maxTokens); err != nil {
		failf("%v", err)
	}
	mk := func(mt int) Provider {
		p, _ := newProvider(pName, pModel, *temp, mt)
		return p
	}

	mapMD, err := os.ReadFile(mapPath(root))
	if err != nil {
		failf("reading map: %v (try %s)", err, cyan("archi init -force"))
	}
	profileMD, err := os.ReadFile(profilePath(root))
	if err != nil {
		failf("reading profile: %v (try %s)", err, cyan("archi init -force"))
	}
	var focusFiles []seedFile
	if len(focus) > 0 {
		focusFiles = collectFocus(root, focus)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	sess := &session{
		ctx: ctx, mk: mk, root: root, cfg: cfg,
		profile: string(profileMD), fileMap: string(mapMD), focus: focusFiles,
		maxTokens: *maxTokens, yes: *yes, htmlFile: *htmlFile,
	}

	interactive := stdoutTTY && isTTY(os.Stdin) && !*noInteractive && *out == ""
	if interactive {
		sess.runInteractive(request)
		return
	}
	sess.runOneShot(request, *out, *build, *noStream)
}

// session carries everything the design lifecycle needs between phases.
type session struct {
	ctx       context.Context
	mk        providerFactory
	root      string
	cfg       *Config
	profile   string
	fileMap   string
	focus     []seedFile
	maxTokens int
	yes       bool
	htmlFile  string
}

func (s *session) design() Provider { return s.mk(s.maxTokens) }
func (s *session) builder() Provider {
	mt := s.maxTokens
	if mt < 16000 {
		mt = 16000 // code generation needs room for full files
	}
	return s.mk(mt)
}

// runInteractive walks interview → design → review loop → build.
func (s *session) runInteractive(request string) {
	prov := s.design()
	banner()
	info("Interviewing with " + prov.Name() + dim("  ·  "+humanCount(s.cfg.FileCount)+" files known"))

	// Phase 1 — clarify.
	sp := startSpinner("Working out what to ask")
	qraw, err := runModel(s.ctx, prov, clarifyPrompt, clarifyUserMessage(s.profile, s.fileMap, s.focus, request), nil, sp)
	if err != nil {
		sp.Abort()
		failf("%v", err)
	}
	sp.Stop("Questions ready")
	answers := askQuestions(parseQuestions(qraw))

	req := request
	if answers != "" {
		req = request + "\n\nClarifications from the requester:\n" + answers
	}

	// Phase 2 — design (streamed live).
	info("Designing …")
	os.Stderr.WriteString("\n")
	doc, err := runModel(s.ctx, prov, architectPrompt, designUserMessage(s.profile, s.fileMap, s.focus, req), os.Stdout, nil)
	if err != nil {
		failf("%v", err)
	}

	// Phase 3 — review loop.
	for {
		switch reviewChoice() {
		case "a":
			s.buildCode(doc)
			s.maybeHTML(doc)
			return
		case "m":
			ch := readLine("what should change?")
			if ch == "" {
				continue
			}
			info("Revising …")
			os.Stderr.WriteString("\n")
			if doc, err = runModel(s.ctx, prov, architectPrompt, reviseUserMessage(doc, ch), os.Stdout, nil); err != nil {
				failf("%v", err)
			}
		case "q":
			qq := readLine("your question:")
			if qq == "" {
				continue
			}
			os.Stderr.WriteString("\n")
			if _, err = runModel(s.ctx, prov, answerPrompt, questionUserMessage(doc, s.profile, qq), os.Stdout, nil); err != nil {
				failf("%v", err)
			}
			os.Stderr.WriteString("\n")
		case "w":
			fn := readLine("filename:")
			if fn == "" {
				fn = "design.md"
			}
			if err := os.WriteFile(fn, []byte(doc), 0o644); err != nil {
				failf("%v", err)
			}
			ok("Wrote " + fn)
		case "x":
			s.maybeHTML(doc)
			ok("Done.")
			return
		default:
			warn("Pick a, m, q, w, or x.")
		}
	}
}

// runOneShot is the scriptable path: design to stdout/-o, optionally build.
func (s *session) runOneShot(request, outPath string, build, noStream bool) {
	prov := s.design()

	var target io.Writer = os.Stdout
	var outFile *os.File
	if outPath != "" {
		var err error
		if outFile, err = os.Create(outPath); err != nil {
			failf("%v", err)
		}
		defer outFile.Close()
		target = outFile
	}

	live := outPath == "" && stdoutTTY && !noStream

	banner()
	info("Designing with " + prov.Name())

	var doc string
	var err error
	if live {
		os.Stderr.WriteString("\n")
		doc, err = runModel(s.ctx, prov, architectPrompt, designUserMessage(s.profile, s.fileMap, s.focus, request), os.Stdout, nil)
		if err != nil {
			failf("%v", err)
		}
	} else {
		sp := startSpinner("Designing")
		doc, err = runModel(s.ctx, prov, architectPrompt, designUserMessage(s.profile, s.fileMap, s.focus, request), nil, sp)
		if err != nil {
			sp.Abort()
			failf("%v", err)
		}
		sp.Stop("Design ready")
		if _, err := io.WriteString(target, doc); err != nil {
			failf("%v", err)
		}
	}
	if outFile != nil {
		ok("Wrote ~" + humanCount(estimateTokens(doc)) + " tokens → " + outPath)
	}
	s.maybeHTML(doc)
	if build {
		s.buildCode(doc)
	}
}

// buildCode generates the implementation from an approved design and applies it,
// streaming a live per-file view so you can watch each file being written.
func (s *session) buildCode(design string) {
	prov := s.builder()
	info("Generating code with " + prov.Name())
	os.Stderr.WriteString("\n")
	cs := newCodeStream()
	code, err := runModel(s.ctx, prov, buildPrompt, buildUserMessage(design, s.profile, s.fileMap), cs, nil)
	cs.flush()
	if err != nil {
		failf("%v", err)
	}
	os.Stderr.WriteString("\n")
	ok("Code generated")
	if err := applyChanges(s.root, parseFileBlocks(code), s.yes); err != nil {
		failf("%v", err)
	}
}

func (s *session) maybeHTML(doc string) {
	if s.htmlFile == "" {
		return
	}
	if err := writeHTMLFile(s.htmlFile, "archi design", doc); err != nil {
		warn("HTML export failed: " + err.Error())
		return
	}
	ok("Rendered → " + s.htmlFile + dim("  (open in a browser for drawn diagrams)"))
}

// resolveInput picks the request text by precedence: args, then file, then stdin.
func resolveInput(args []string, fileData []byte, hasFile bool, stdinR io.Reader, stdinIsTTY bool) (string, error) {
	if j := strings.TrimSpace(strings.Join(args, " ")); j != "" {
		return j, nil
	}
	if hasFile {
		if str := strings.TrimSpace(string(fileData)); str != "" {
			return str, nil
		}
	}
	if !stdinIsTTY {
		b, _ := io.ReadAll(stdinR)
		if str := strings.TrimSpace(string(b)); str != "" {
			return str, nil
		}
	}
	return "", errors.New("no request given — pass it as an argument, with -f <file>, or on stdin")
}

// collectFocus reads files matching the given globs as extra grounding.
func collectFocus(root string, globs []string) []seedFile {
	files, err := scanRepo(root)
	if err != nil {
		return nil
	}
	fg := parseGitignore(strings.NewReader(strings.Join(globs, "\n")))
	var out []seedFile
	used := 0
	for _, f := range files {
		if !fg.Match(f.rel, false) {
			continue
		}
		data, okRead := readTextFile(root, f.rel, 256*1024)
		if !okRead {
			continue
		}
		t := estimateTokens(string(data))
		if used+t > 60000 {
			warn("focus budget reached — some matched files were skipped")
			break
		}
		used += t
		out = append(out, seedFile{rel: f.rel, lang: f.lang, content: string(data)})
	}
	if len(out) > 0 {
		info("Focused on " + humanCount(len(out)) + " files (~" + humanCount(used) + " tokens)")
	}
	return out
}
