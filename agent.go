package main

import (
	"sort"
	"strings"
	"text/template"
	"time"
)

// AgentRole identifies a specialist. Roles are data, not code.
type AgentRole string

const (
	RoleExplorer   AgentRole = "explorer"
	RoleDesigner   AgentRole = "designer"
	RoleSecCritic  AgentRole = "sec-critic"
	RoleSimpCritic AgentRole = "simplicity-critic"
	RoleGrndCritic AgentRole = "grounding-critic"
	RolePlanner    AgentRole = "planner"
	RoleBuilder    AgentRole = "builder"
	RoleIntegrator AgentRole = "integrator"
	RoleVerifier   AgentRole = "verifier"
	RoleRepairer   AgentRole = "repairer"
	RoleReviewer   AgentRole = "reviewer"
	RoleTestWriter AgentRole = "test-writer"
	RoleDocWriter  AgentRole = "doc-writer"
	RoleSummarizer AgentRole = "summarizer"
)

// AgentSpec is the static definition of a role: how it prompts and how much it
// may spend. Specs are registered in the package-level agentSpecs table.
type AgentSpec struct {
	Role      AgentRole
	System    string  // system prompt template (text/template, data = TaskInput)
	MaxTokens int     // output budget for one call
	Temp      float64 // 0 for critics/verifiers, 0.4 designer default
}

// Agent binds a spec to a live provider (possibly a cheaper model for critics).
type Agent struct {
	Spec AgentSpec
	P    Provider // the existing Provider interface — unchanged
}

// TaskInput is everything an agent call may draw on. Grounding reuses the
// existing budgetPart type from budget.go; fitBudget applies before send.
type TaskInput struct {
	Request   string            // the user's design/build/review request
	Interview string            // Q&A transcript, "" when not applicable
	Grounding []budgetPart      // profile, map, explorer brief, focus files…
	Extra     map[string]string // phase-specific payloads (design doc, packet, diff…)
}

// AgentResult is one completed agent call.
type AgentResult struct {
	Role      AgentRole
	Output    string   // raw text/markdown output
	Verdict   *Verdict // parsed, critics/verifiers only, else nil
	TokensIn  int      // estimated via estimateTokens
	TokensOut int
	Duration  time.Duration
	Err       error
}

// Verdict is a critic's or verifier's structured judgment.
type Verdict struct {
	Status   VerdictStatus
	Findings []Finding
}

// VerdictStatus grades a review outcome.
type VerdictStatus string

const (
	VerdictClean    VerdictStatus = "clean"
	VerdictAdvisory VerdictStatus = "advisory"
	VerdictBlocking VerdictStatus = "blocking"
)

// Finding is one numbered issue. Paths is mandatory for the grounding critic (a
// finding with no path is downgraded to advisory during parsing).
type Finding struct {
	Severity VerdictStatus // blocking or advisory
	Title    string
	Detail   string
	Paths    []string
}

// agentSpecs registers the static definition of every role. Phase 1 fills in
// designer and builder; later phases add rows.
var agentSpecs = map[AgentRole]AgentSpec{
	RoleDesigner: {Role: RoleDesigner, System: architectPrompt, MaxTokens: 8000, Temp: 0.4},
	RoleBuilder:  {Role: RoleBuilder, System: buildPrompt, MaxTokens: 16000, Temp: 0.4},
}

// specFor returns the registered spec for role and whether one exists.
func specFor(role AgentRole) (AgentSpec, bool) {
	spec, ok := agentSpecs[role]
	return spec, ok
}

// renderSystem executes spec.System as a text/template with data = in. A prompt
// with no template actions passes through byte-identically (all current prompt
// constants contain no actions). Parse or execute errors are returned, never
// panicked.
func renderSystem(spec AgentSpec, in TaskInput) (string, error) {
	tmpl, err := template.New(string(spec.Role)).Parse(spec.System)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, in); err != nil {
		return "", err
	}
	return b.String(), nil
}

// agentUserMessage assembles the user message for an agent call. When
// in.Extra["user_message"] is set it is returned verbatim — the escape hatch
// that lets cmd_design keep its existing prompt-builder output byte-identical.
// Otherwise a generic assembly is produced: one "=== <UPPERCASED PART NAME>
// ===" section per Grounding part (in order), then "=== INTERVIEW ===" when
// Interview != "", then one section per Extra payload (sorted by key, the
// user_message hatch excluded), then "=== REQUEST ===".
func agentUserMessage(in TaskInput) string {
	if in.Extra != nil {
		if m, ok := in.Extra["user_message"]; ok {
			return m
		}
	}
	var b strings.Builder
	for _, p := range in.Grounding {
		b.WriteString("=== " + strings.ToUpper(p.name) + " ===\n\n")
		b.WriteString(p.content)
		b.WriteString("\n\n")
	}
	if in.Interview != "" {
		b.WriteString("=== INTERVIEW ===\n\n")
		b.WriteString(in.Interview)
		b.WriteString("\n\n")
	}
	if len(in.Extra) > 0 {
		keys := make([]string, 0, len(in.Extra))
		for k := range in.Extra {
			if k != "user_message" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("=== " + strings.ToUpper(k) + " ===\n\n")
			b.WriteString(in.Extra[k])
			b.WriteString("\n\n")
		}
	}
	b.WriteString("=== REQUEST ===\n\n")
	b.WriteString(in.Request)
	return b.String()
}
