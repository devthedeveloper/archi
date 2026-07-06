package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"
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
	agents := fs.String("agents", "", "ensemble mode: full, fast, or off (default: from .archi, else full)")
	criticModel := fs.String("critic-model", "", "cheaper model for critics (default: the design model)")
	rounds := fs.Int("rounds", 0, "designer↔critic debate rounds, 1-3 (default: from .archi, else 1)")
	swarm := fs.String("swarm", "", "split the build into parallel packets: on|off (default: on with -agents full)")
	verifyFlag := fs.String("verify", "on", "run the repo's toolchain after applying: on|off")
	maxRepair := fs.Int("max-repair", 3, "max verify/repair iterations")
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

	// Resolve ensemble options (flag > config > default) and persist any flag
	// the user explicitly set, so it becomes the default next run.
	opts := resolveEnsembleOpts(cfg, *agents, *criticModel, *rounds)
	setA, setC, setR := false, false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "agents":
			setA = true
		case "critic-model":
			setC = true
		case "rounds":
			setR = true
		}
	})
	persistEnsembleFlags(cfg, root, setA, setC, setR, *agents, *criticModel, *rounds)
	// Critics run at temperature 0, on their own model when -critic-model is set.
	criticMk := func(mt int) Provider {
		cm := opts.criticModel
		if cm == "" {
			cm = pModel
		}
		p, _ := newProvider(pName, cm, 0, mt)
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

	// Staleness is advisory: warn when the repo drifted from the cache, but
	// design anyway — an approximate profile still beats none.
	if d, fresh := checkFreshness(root); fresh && !d.empty() {
		warn("repo has changed since init: " + d.summary() + " — consider " + cyan("archi init -refresh"))
	}

	// Fit the grounding into the context budget: the profile always ships
	// whole; the map and focus files shrink or drop to fit.
	profile, fileMap, fitFocus := fitGrounding(string(profileMD), string(mapMD), focusFiles)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	sess := &session{
		ctx: ctx, mk: mk, root: root, cfg: cfg,
		profile: profile, fileMap: fileMap, focus: fitFocus,
		maxTokens: *maxTokens, yes: *yes, htmlFile: *htmlFile,
		opts: opts, criticMk: criticMk,
		swarm:     *swarm == "on" || (*swarm == "" && opts.mode == "full"),
		verify:    *verifyFlag != "off",
		maxRepair: *maxRepair,
	}
	sess.orch = newOrchestrator(root)

	interactive := stdoutTTY && isTTY(os.Stdin) && !*noInteractive && *out == ""
	if interactive {
		sess.runInteractive(request)
		return
	}
	sess.runOneShot(request, *out, *build, *noStream)
}

// session carries everything the design lifecycle needs between phases.
type session struct {
	ctx         context.Context
	mk          providerFactory
	root        string
	cfg         *Config
	profile     string
	fileMap     string
	focus       []seedFile
	maxTokens   int
	yes         bool
	htmlFile    string
	historyPath string        // where this run's design history entry lives, "" if none
	orch        *Orchestrator // agent runtime: traces every call, charges the run budget
	opts        ensembleOpts  // resolved -agents/-critic-model/-rounds
	criticMk    providerFactory
	swarm       bool // split the build into parallel packets
	verify      bool // run the toolchain after applying
	maxRepair   int  // max verify/repair iterations
}

// agentFor binds a registered spec to the session's provider for that role
// (designer budget for design-side calls, builder budget for build), with an
// optional system-prompt override for the interview/answer sub-calls.
func (s *session) agentFor(role AgentRole, systemOverride string) Agent {
	spec, _ := specFor(role)
	if systemOverride != "" {
		spec.System = systemOverride
	}
	p := s.design()
	if role == RoleBuilder {
		p = s.builder()
	}
	return Agent{Spec: spec, P: p}
}

// designTask wraps a prebuilt user message in a TaskInput so the orchestrator
// sends it byte-identically (the Phase 1 escape hatch).
func designTask(request, userMessage string) TaskInput {
	return TaskInput{Request: request, Extra: map[string]string{"user_message": userMessage}}
}

// printCost prints the orchestrator's end-of-run cost summary, if any calls ran.
func (s *session) printCost() {
	if line := s.orch.costLine(); line != "" {
		info(line)
	}
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
	res := s.orch.runStream(s.ctx, s.agentFor(RoleDesigner, clarifyPrompt),
		designTask(request, clarifyUserMessage(s.profile, s.fileMap, s.focus, request)), nil, sp)
	if res.Err != nil {
		sp.Abort()
		failf("%v", res.Err)
	}
	sp.Stop("Questions ready")
	answers := askQuestions(parseQuestions(res.Output))

	// Phase 2 — design. Full mode runs the ensemble; fast/off is byte-identical.
	var doc string
	if s.opts.mode == "full" {
		var derr error
		if doc, derr = s.designEnsemble(request, answers, os.Stdout); derr != nil {
			failf("%v", derr)
		}
	} else {
		req := request
		if answers != "" {
			req = request + "\n\nClarifications from the requester:\n" + answers
		}
		info("Designing …")
		os.Stderr.WriteString("\n")
		res = s.orch.runStream(s.ctx, s.agentFor(RoleDesigner, ""),
			designTask(request, designUserMessage(s.profile, s.fileMap, "", s.focus, req)), os.Stdout, nil)
		if res.Err != nil {
			failf("%v", res.Err)
		}
		doc = res.Output
	}
	s.recordDesign(request, prov.Name(), doc)

	// Phase 3 — review loop.
	for {
		switch reviewChoice() {
		case "a":
			s.buildCode(doc)
			s.maybeHTML(doc)
			s.printCost()
			return
		case "m":
			ch := readLine("what should change?")
			if ch == "" {
				continue
			}
			info("Revising …")
			os.Stderr.WriteString("\n")
			res = s.orch.runStream(s.ctx, s.agentFor(RoleDesigner, ""),
				designTask(request, reviseUserMessage(doc, ch)), os.Stdout, nil)
			if res.Err != nil {
				failf("%v", res.Err)
			}
			doc = res.Output
			s.recordDesign(request, prov.Name(), doc)
		case "q":
			qq := readLine("your question:")
			if qq == "" {
				continue
			}
			os.Stderr.WriteString("\n")
			res = s.orch.runStream(s.ctx, s.agentFor(RoleDesigner, answerPrompt),
				designTask(request, questionUserMessage(doc, s.profile, qq)), os.Stdout, nil)
			if res.Err != nil {
				failf("%v", res.Err)
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
			s.printCost()
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
	if s.opts.mode == "full" {
		var liveW io.Writer
		if live {
			liveW = os.Stdout
			os.Stderr.WriteString("\n")
		}
		var derr error
		if doc, derr = s.designEnsemble(request, "", liveW); derr != nil {
			failf("%v", derr)
		}
		if !live {
			if _, err := io.WriteString(target, doc); err != nil {
				failf("%v", err)
			}
		}
	} else {
		task := designTask(request, designUserMessage(s.profile, s.fileMap, "", s.focus, request))
		if live {
			os.Stderr.WriteString("\n")
			res := s.orch.runStream(s.ctx, s.agentFor(RoleDesigner, ""), task, os.Stdout, nil)
			if res.Err != nil {
				failf("%v", res.Err)
			}
			doc = res.Output
		} else {
			sp := startSpinner("Designing")
			res := s.orch.runStream(s.ctx, s.agentFor(RoleDesigner, ""), task, nil, sp)
			if res.Err != nil {
				sp.Abort()
				failf("%v", res.Err)
			}
			sp.Stop("Design ready")
			doc = res.Output
			if _, err := io.WriteString(target, doc); err != nil {
				failf("%v", err)
			}
		}
	}
	if outFile != nil {
		ok("Wrote ~" + humanCount(estimateTokens(doc)) + " tokens → " + outPath)
	}
	s.recordDesign(request, prov.Name(), doc)
	s.maybeHTML(doc)
	if build {
		s.buildCode(doc)
	}
	s.printCost()
}

// recordDesign saves (or refreshes) the design in .archi/designs. History is
// advisory: failures warn and never interrupt the flow.
func (s *session) recordDesign(request, provider, doc string) {
	if s.historyPath == "" {
		s.historyPath = designHistoryPath(s.root, request, time.Now())
	}
	if err := writeDesignHistory(s.historyPath, request, provider, time.Now(), false, doc); err != nil {
		warn("could not save design history: " + err.Error())
		s.historyPath = ""
	}
}

// buildCode turns an approved design into code. It dispatches to the parallel
// swarm for big designs, else the single-shot builder, then verifies+repairs.
func (s *session) buildCode(design string) {
	if s.historyPath != "" {
		if err := markDesignBuilt(s.historyPath); err != nil {
			warn("could not update design history: " + err.Error())
		}
	}
	if s.swarm && shouldSwarm(design) {
		s.buildSwarm(design)
		return
	}
	s.buildSingleShot(design)
}

// buildSingleShot generates all files in one call, streaming a live per-file
// view, applies them, and verifies.
func (s *session) buildSingleShot(design string) {
	prov := s.builder()
	info("Generating code with " + prov.Name())
	os.Stderr.WriteString("\n")
	cs := newCodeStream()
	res := s.orch.runStream(s.ctx, s.agentFor(RoleBuilder, ""),
		designTask(design, buildUserMessage(design, s.profile, s.fileMap)), cs, nil)
	cs.flush()
	if res.Err != nil {
		failf("%v", res.Err)
	}
	os.Stderr.WriteString("\n")
	ok("Code generated")
	s.applyAndVerify(design, parseFileBlocks(res.Output))
}

// buildSwarm plans the design into packets, builds them in parallel waves,
// merges, applies, and verifies. Any planning/swarm failure falls back to the
// single-shot path.
func (s *session) buildSwarm(design string) {
	sp := startSpinner("Planning build with " + s.builder().Name())
	planAgent := Agent{Spec: AgentSpec{Role: RolePlanner, System: plannerPrompt, MaxTokens: 2000, Temp: 0}, P: s.mk(2000)}
	res := s.orch.runStream(s.ctx, planAgent, designTask(design, plannerUserMessage(design, s.profile, s.fileMap)), nil, sp)
	if res.Err != nil {
		sp.Abort()
		warn("planning failed: " + res.Err.Error() + " — falling back to single-shot build")
		s.buildSingleShot(design)
		return
	}
	packets, err := parsePackets(res.Output)
	if err == nil {
		err = validatePackets(packets)
	}
	if err == nil {
		_, err = topoLevels(packets)
	}
	if err != nil {
		sp.Abort()
		warn("planning failed: " + err.Error() + " — falling back to single-shot build")
		s.buildSingleShot(design)
		return
	}
	sp.Stop(fmt.Sprintf("Plan: %d packets", len(packets)))
	printPlan(packets)

	merged, results, serr := runSwarm(s.ctx, s.orch, s, design, packets)
	if serr != nil {
		warn("swarm build failed: " + serr.Error() + " — falling back to single-shot build")
		s.buildSingleShot(design)
		return
	}
	reportPackets(results)
	s.applyAndVerify(design, merged)
}

// applyAndVerify applies the generated changes and, on success, runs the
// verify/repair loop when -verify is on.
func (s *session) applyAndVerify(design string, changes []fileChange) {
	applied, stamp, err := applyChanges(s.root, changes, s.yes)
	if err != nil {
		failf("%v", err)
	}
	if applied && s.verify {
		s.repairLoop(s.ctx, s.orch, design, stamp)
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
		out = append(out, seedFile{rel: f.rel, lang: f.lang, content: redactForPrompt(f.rel, string(data))})
	}
	if len(out) > 0 {
		info("Focused on " + humanCount(len(out)) + " files (~" + humanCount(used) + " tokens)")
	}
	return out
}
