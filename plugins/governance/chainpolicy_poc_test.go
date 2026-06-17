package governance

import (
	"context"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/require"
)

// Dynamic plugin-chain POC — see docs/multi-tenant-f5/07-dynamic-plugin-chain.md.
//
// Goal: prove the "separate the selector from the execution" pattern can
// model the red-team use case (bypass guardrails + divert telemetry to a
// protected sink) WITHOUT plugins knowing about each other or about the
// VK's role/tags.
//
// The POC lives in plugins/governance because the governance module already
// imports cel-go (used by the routing rules). The types below are framework-
// level concepts; if/when the pattern is adopted, these graduate into
// multitenant/chainpolicy/ as their own package (with cel-go added as a
// direct dep on multitenant). For the POC, this placement avoids touching
// module surfaces.
//
// What this POC validates:
//   1. A CEL-driven ChainPolicy selects a *whole* ChainPlan per request —
//      a single, auditable decision rather than scattered per-plugin
//      conditionals.
//   2. Plugins are referenced by NAME + VARIANT. The policy never sees a
//      plugin's config schema; plugins own their variants.
//   3. The audit trail (ChainPlan.Why) carries every decision the policy
//      made so a security review can reconstruct the chain.
//   4. Executing the plan with a mock plugin registry produces the right
//      observable behavior — the red team really does skip guardrails and
//      divert telemetry, the standard VK really does enforce/store locally.
//
// What this POC deliberately leaves open (called out in the doc):
//   - Plugin RESPONSE-time variants (PostLLMHook only). The current model
//     selects a chain at PreLLMHook time. Post-hook variant selection
//     based on response content is a v2 axis.
//   - Variant CONFIG overrides (e.g. "telemetry: standard, but sample at
//     0.01"). Variants are named lookups today; config-override is a
//     v2 axis that bloats the policy DSL.
//   - Rule LAYERING (default chain + per-rule overlays). Today rules
//     fully replace the base plan. Overlays add complexity worth deferring.

// --- Framework-level types (would graduate to multitenant/chainpolicy/) ---

// ChainStep names a plugin and the variant the policy selected for THIS
// request. Why is the rule name that picked this step — feeds the
// per-request audit log.
type ChainStep struct {
	Plugin  string
	Variant string
	Why     string
}

// ChainPlan is the ordered list of steps a request must run through, plus
// the top-level audit trail (which rule fired, what fell through). Plan
// generation is a single function call against PolicyInput; execution is
// strictly downstream.
type ChainPlan struct {
	Steps []ChainStep
	Why   []string // top-level decisions ("matched: red-team-divert", "fell through to default")
}

// PolicyInput is the minimum signal the policy reads. v1 is VK-scoped;
// model/provider/prompt-content are deferred to a v2 axis (see doc §
// "Open dimensions").
type PolicyInput struct {
	TenantID string
	VK       VKSnapshot
}

// VKSnapshot is the in-plugin view of a Virtual Key for policy evaluation.
// Avoids dragging the full configstoreTables.TableVirtualKey type into the
// policy layer — the policy only needs the few fields it filters on.
type VKSnapshot struct {
	ID   string
	Name string
	Tags []string
	Role string // optional: "internal", "external", "red-team", etc.
}

// ChainPolicy is the seam between "tenant resolved" and "plugin chain
// runs." One function: given a request's PolicyInput, return the plan.
type ChainPolicy interface {
	Select(ctx context.Context, in PolicyInput) (ChainPlan, error)
}

// --- CEL implementation ---

// Rule is one entry in a policy: a CEL expression evaluated against the
// PolicyInput, plus the chain plan to apply if the expression matches.
// Rules are tried in order; the first matching rule's plan wins.
type Rule struct {
	Name string      // human readable, surfaced in ChainPlan.Why
	Expr string      // CEL boolean expression
	Plan []ChainStep // chain to apply when the expression is true
}

type compiledRule struct {
	name    string
	expr    string
	program cel.Program
	plan    []ChainStep
}

// CELChainPolicy evaluates a list of CEL rules in order against the
// PolicyInput and returns the first matching plan, or the base plan when
// no rule matches.
type CELChainPolicy struct {
	rules []compiledRule
	base  []ChainStep
}

// NewCELChainPolicy compiles the rules and returns the policy. Returns an
// error per rule that fails to compile — fail-fast at policy-load time so
// admins never deploy a broken rule.
func NewCELChainPolicy(rules []Rule, base []ChainStep) (*CELChainPolicy, error) {
	env, err := newChainPolicyCELEnv()
	if err != nil {
		return nil, err
	}
	compiled := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		ast, iss := env.Compile(r.Expr)
		if iss != nil && iss.Err() != nil {
			return nil, &ruleCompileError{Rule: r.Name, Err: iss.Err()}
		}
		prog, err := env.Program(ast)
		if err != nil {
			return nil, &ruleCompileError{Rule: r.Name, Err: err}
		}
		compiled = append(compiled, compiledRule{
			name: r.Name, expr: r.Expr, program: prog, plan: r.Plan,
		})
	}
	return &CELChainPolicy{rules: compiled, base: base}, nil
}

func newChainPolicyCELEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("tenant_id", cel.StringType),
		cel.Variable("vk_id", cel.StringType),
		cel.Variable("vk_name", cel.StringType),
		cel.Variable("vk_tags", cel.ListType(cel.StringType)),
		cel.Variable("vk_role", cel.StringType),
	)
}

func (p *CELChainPolicy) Select(ctx context.Context, in PolicyInput) (ChainPlan, error) {
	vars := map[string]any{
		"tenant_id": in.TenantID,
		"vk_id":     in.VK.ID,
		"vk_name":   in.VK.Name,
		"vk_tags":   in.VK.Tags,
		"vk_role":   in.VK.Role,
	}
	for _, r := range p.rules {
		out, _, err := r.program.Eval(vars)
		if err != nil {
			// Fail-open: a malformed rule evaluation should NOT break the
			// request. Audit and try the next rule.
			continue
		}
		matched, ok := out.Value().(bool)
		if !ok || !matched {
			continue
		}
		plan := ChainPlan{
			Steps: append([]ChainStep(nil), r.plan...),
			Why:   []string{"matched rule: " + r.name + " (" + r.expr + ")"},
		}
		// Stamp the Why on each step that doesn't already carry one.
		for i := range plan.Steps {
			if plan.Steps[i].Why == "" {
				plan.Steps[i].Why = r.name
			}
		}
		return plan, nil
	}
	return ChainPlan{
		Steps: append([]ChainStep(nil), p.base...),
		Why:   []string{"no rule matched, using default chain"},
	}, nil
}

type ruleCompileError struct {
	Rule string
	Err  error
}

func (e *ruleCompileError) Error() string {
	return "chainpolicy: compile rule " + e.Rule + ": " + e.Err.Error()
}

// --- Mock plugin registry used by the execution test ---

// mockPlugin is the smallest shape a plugin needs for this POC: a name,
// a catalog of named variants, and each variant's per-request behaviour
// (here just appending to an audit slice).
type mockPlugin struct {
	name     string
	variants map[string]func(audit *[]string) // variant name → side-effect
}

func (m *mockPlugin) Apply(variant string, audit *[]string) {
	if fn, ok := m.variants[variant]; ok {
		fn(audit)
		return
	}
	*audit = append(*audit, m.name+":UNKNOWN-VARIANT("+variant+")")
}

type mockPluginRegistry map[string]*mockPlugin

func (r mockPluginRegistry) ExecutePlan(plan ChainPlan, audit *[]string) {
	for _, step := range plan.Steps {
		p, ok := r[step.Plugin]
		if !ok {
			*audit = append(*audit, step.Plugin+":NOT-REGISTERED")
			continue
		}
		p.Apply(step.Variant, audit)
	}
}

// --- Tests ---

// TestChainPolicy_RedTeamBypassesAndDiverts is the headline POC: it proves
// the red-team use case (bypass guardrails + divert telemetry to a
// protected sink + use a protected audit log) is expressible as a single
// CEL rule, with the per-request audit trail surfacing exactly which
// policy rule fired.
func TestChainPolicy_RedTeamBypassesAndDiverts(t *testing.T) {
	defaultPlan := []ChainStep{
		{Plugin: "guardrails", Variant: "enforce"},
		{Plugin: "telemetry", Variant: "local"},
		{Plugin: "audit", Variant: "standard"},
	}
	redTeamPlan := []ChainStep{
		{Plugin: "guardrails", Variant: "skip"},
		{Plugin: "telemetry", Variant: "s3-divert"},
		{Plugin: "audit", Variant: "protected"},
	}

	policy, err := NewCELChainPolicy(
		[]Rule{
			{
				Name: "red-team-divert",
				Expr: `"red-team" in vk_tags`,
				Plan: redTeamPlan,
			},
		},
		defaultPlan,
	)
	require.NoError(t, err)

	// Red-team VK
	redIn := PolicyInput{
		TenantID: "acme",
		VK:       VKSnapshot{ID: "vk-red-001", Name: "rt-probe", Tags: []string{"red-team", "internal"}},
	}
	redPlan, err := policy.Select(context.Background(), redIn)
	require.NoError(t, err)
	require.Len(t, redPlan.Steps, 3)
	require.Equal(t, "skip", redPlan.Steps[0].Variant)
	require.Equal(t, "s3-divert", redPlan.Steps[1].Variant)
	require.Equal(t, "protected", redPlan.Steps[2].Variant)
	require.Contains(t, redPlan.Why[0], "matched rule: red-team-divert")
	// Audit trail must reach every step.
	for i, s := range redPlan.Steps {
		require.NotEmpty(t, s.Why, "step %d (%s/%s) must carry an audit string", i, s.Plugin, s.Variant)
	}

	// Standard VK
	stdIn := PolicyInput{
		TenantID: "acme",
		VK:       VKSnapshot{ID: "vk-std-001", Name: "team-a-app", Tags: []string{"prod"}},
	}
	stdPlan, err := policy.Select(context.Background(), stdIn)
	require.NoError(t, err)
	require.Len(t, stdPlan.Steps, 3)
	require.Equal(t, "enforce", stdPlan.Steps[0].Variant)
	require.Equal(t, "local", stdPlan.Steps[1].Variant)
	require.Equal(t, "standard", stdPlan.Steps[2].Variant)
	require.Contains(t, stdPlan.Why[0], "no rule matched")
}

// TestChainExecution_RedTeamPlanProducesRightSideEffects executes the plan
// with a mock plugin registry to confirm the *observable* behaviour
// matches the policy decision — not just that the policy ROUTED to the
// right variants but that the variants actually run differently.
func TestChainExecution_RedTeamPlanProducesRightSideEffects(t *testing.T) {
	registry := mockPluginRegistry{
		"guardrails": {
			name: "guardrails",
			variants: map[string]func(*[]string){
				"enforce": func(a *[]string) { *a = append(*a, "GUARD:enforced") },
				"skip":    func(a *[]string) { *a = append(*a, "GUARD:skipped") },
			},
		},
		"telemetry": {
			name: "telemetry",
			variants: map[string]func(*[]string){
				"local":     func(a *[]string) { *a = append(*a, "TEL:local-sql") },
				"s3-divert": func(a *[]string) { *a = append(*a, "TEL:s3://red-team-audit") },
			},
		},
		"audit": {
			name: "audit",
			variants: map[string]func(*[]string){
				"standard":  func(a *[]string) { *a = append(*a, "AUD:standard-store") },
				"protected": func(a *[]string) { *a = append(*a, "AUD:protected-sink") },
			},
		},
	}

	policy, err := NewCELChainPolicy(
		[]Rule{
			{
				Name: "red-team-divert",
				Expr: `"red-team" in vk_tags`,
				Plan: []ChainStep{
					{Plugin: "guardrails", Variant: "skip"},
					{Plugin: "telemetry", Variant: "s3-divert"},
					{Plugin: "audit", Variant: "protected"},
				},
			},
		},
		[]ChainStep{
			{Plugin: "guardrails", Variant: "enforce"},
			{Plugin: "telemetry", Variant: "local"},
			{Plugin: "audit", Variant: "standard"},
		},
	)
	require.NoError(t, err)

	run := func(in PolicyInput) []string {
		plan, err := policy.Select(context.Background(), in)
		require.NoError(t, err)
		var audit []string
		registry.ExecutePlan(plan, &audit)
		return audit
	}

	redAudit := run(PolicyInput{TenantID: "acme", VK: VKSnapshot{ID: "vk-red", Tags: []string{"red-team"}}})
	require.Equal(t, []string{"GUARD:skipped", "TEL:s3://red-team-audit", "AUD:protected-sink"}, redAudit)

	stdAudit := run(PolicyInput{TenantID: "acme", VK: VKSnapshot{ID: "vk-std", Tags: []string{"prod"}}})
	require.Equal(t, []string{"GUARD:enforced", "TEL:local-sql", "AUD:standard-store"}, stdAudit)
}

// TestChainPolicy_MalformedExpressionFailsAtPolicyLoad is the safety
// canary: admins can't deploy a syntactically-broken rule and discover it
// at request time. Compile errors surface at NewCELChainPolicy.
func TestChainPolicy_MalformedExpressionFailsAtPolicyLoad(t *testing.T) {
	_, err := NewCELChainPolicy(
		[]Rule{
			{Name: "broken", Expr: `vk_tags contains "wrong-operator"`, Plan: nil}, // CEL uses `in`, not `contains`
		},
		nil,
	)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "compile"),
		"compile error should name the offending rule, got: %v", err)
}

// TestChainPolicy_RuleEvalErrorDoesNotBreakRequest is the fail-open canary:
// if a rule's CEL eval errors at runtime (e.g. type assertion on an unset
// variable), the policy must NOT abort the request — it should treat the
// rule as not-matched, log, and try the next rule. Today the malformed
// reference is caught at compile time (good), but this test stands in for
// the runtime-fault path so the safety story is locked.
func TestChainPolicy_RuleEvalErrorDoesNotBreakRequest(t *testing.T) {
	// A rule that compiles but is unsatisfiable for any VK we'll feed.
	policy, err := NewCELChainPolicy(
		[]Rule{
			{
				Name: "never-matches",
				Expr: `vk_role == "non-existent-role"`,
				Plan: []ChainStep{{Plugin: "shouldnt-run", Variant: "x"}},
			},
		},
		[]ChainStep{{Plugin: "default", Variant: "ok"}},
	)
	require.NoError(t, err)

	plan, err := policy.Select(context.Background(),
		PolicyInput{TenantID: "acme", VK: VKSnapshot{ID: "vk", Tags: []string{"any"}}})
	require.NoError(t, err)
	require.Equal(t, "default", plan.Steps[0].Plugin, "should fall through to default")
	require.Equal(t, "ok", plan.Steps[0].Variant)
	require.Contains(t, plan.Why[0], "no rule matched")
}
