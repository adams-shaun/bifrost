package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Concrete examples for the hybrid named-variant + typed-config model
// proposed in docs/multi-tenant-f5/07-dynamic-plugin-chain.md.
//
// The chain-policy POC (chainpolicy_poc_test.go) validates the *selector*
// pattern abstractly: ChainPolicy.Select → ChainPlan → execute. This file
// fills in what the *plugin side* looks like once the model is real.
//
// Three example plugins are modeled here at realistic config-schema
// fidelity (telemetry, guardrails, audit). Each:
//   - declares a small variant catalog
//   - declares each variant's typed config struct
//   - marks which config keys are tenant-only (set at TenantLoader, NEVER
//     overridable per request) vs. request-overridable (policy may supply
//     via ChainStep.Config)
//
// Two end-to-end tests exercise the model:
//   1. The red-team divert scenario: tenant baseline supplies telemetry
//      endpoint + audit sink; policy supplies divert overrides via the
//      ChainStep.Config map; the merged typed config drives observable
//      behavior.
//   2. A "low-trust customer" scenario: a different rule supplies
//      different overrides on the same plugins, showing the per-request
//      branching is data, not code.
//
// What the examples DELIBERATELY don't model (out of scope for the design
// validation, called out in doc 07):
//   - Real provider integrations (S3 SDK, Prometheus client, etc.) — the
//     side effects here are captured in an in-memory eventLog.
//   - The actual bifrost Pre/Post LLM hook signatures. RunVariant below is
//     a thin stand-in for the eventual variant-dispatch shim (patch20
//     in the doc's series outline).
//   - Plugin lifecycle (Init, Cleanup, Reconfigure). The examples are
//     stateless on purpose; real plugins layer state on top.

// =============================================================================
// Framework-level interfaces (would graduate to multitenant/chainpolicy/)
// =============================================================================

// VariantSchema describes one named variant of a plugin: its typed config
// (as a pointer to a zero-value struct used for json.Unmarshal decode) and
// which keys of that schema are tenant-only.
type VariantSchema struct {
	Name           string
	NewConfig      func() any // returns a *struct to decode into via json.Unmarshal
	TenantOnlyKeys []string   // keys policy.Config MUST NOT set; tenant baseline only
}

// ExamplePlugin is the minimum interface for the variant-dispatch model.
// Real plugins would also implement schemas.LLMPlugin; this is the
// variant-selection slice.
type ExamplePlugin interface {
	Name() string
	Variants() []VariantSchema
	RunVariant(ctx context.Context, req *exampleRequest, variant string, cfg any, log *eventLog) error
}

// exampleRequest is the minimum request shape these examples need.
type exampleRequest struct {
	RequestID string
	TenantID  string
	VKID      string
	Prompt    string
	Response  string
}

// eventLog captures plugin side effects so tests can assert on them.
type eventLog struct {
	events []string
}

func (e *eventLog) Add(format string, a ...any) {
	e.events = append(e.events, fmt.Sprintf(format, a...))
}

// =============================================================================
// Policy validation: decode each step's config against the variant's schema
// =============================================================================

// ValidatePolicyAgainstPlugins runs at NewCELChainPolicy time (well,
// alongside it — see the test that calls this). For each step in each
// rule's plan AND the base plan, it:
//   - confirms the plugin exists
//   - confirms the variant exists
//   - confirms the supplied config decodes into the variant's typed schema
//   - confirms the policy is not trying to set a tenant-only key
//
// This is the choke point that makes the hybrid model auditable: admins
// can't deploy a policy with a typo'd variant name or a config key the
// variant doesn't understand.
func ValidatePolicyAgainstPlugins(rules []Rule, basePlan []ChainStep, plugins map[string]ExamplePlugin) error {
	checkPlan := func(planName string, plan []ChainStep) error {
		for i, step := range plan {
			p, ok := plugins[step.Plugin]
			if !ok {
				return fmt.Errorf("%s step %d: unknown plugin %q", planName, i, step.Plugin)
			}
			variant, ok := findVariant(p.Variants(), step.Variant)
			if !ok {
				known := make([]string, 0, len(p.Variants()))
				for _, v := range p.Variants() {
					known = append(known, v.Name)
				}
				return fmt.Errorf("%s step %d: plugin %q has no variant %q (known: %v)", planName, i, step.Plugin, step.Variant, known)
			}
			// Validate config: it must decode into the variant's schema AND
			// must not touch tenant-only keys.
			if step.Config != nil {
				for k := range step.Config {
					for _, tok := range variant.TenantOnlyKeys {
						if k == tok {
							return fmt.Errorf("%s step %d: policy attempts to override tenant-only key %q on %s/%s", planName, i, k, step.Plugin, step.Variant)
						}
					}
				}
				if err := decodeConfig(step.Config, variant.NewConfig()); err != nil {
					return fmt.Errorf("%s step %d: %s/%s config: %w", planName, i, step.Plugin, step.Variant, err)
				}
			}
		}
		return nil
	}
	for _, r := range rules {
		if err := checkPlan("rule "+r.Name, r.Plan); err != nil {
			return err
		}
	}
	if err := checkPlan("base plan", basePlan); err != nil {
		return err
	}
	return nil
}

func findVariant(variants []VariantSchema, name string) (VariantSchema, bool) {
	for _, v := range variants {
		if v.Name == name {
			return v, true
		}
	}
	return VariantSchema{}, false
}

// decodeConfig does a strict JSON round-trip to surface unknown keys + type
// mismatches at validation time. Strict mode (DisallowUnknownFields) is the
// teeth — without it, the admin can typo a key and the policy still loads.
func decodeConfig(cfg map[string]any, into any) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// =============================================================================
// Per-request merge: tenant baseline + policy override
// =============================================================================

// mergeConfig produces the final typed config the plugin sees at request
// time. Order: tenant-baseline first, then policy.Config overlays on top
// (only for keys that aren't tenant-only — which is already enforced at
// policy-validation time, so this just trusts that property).
//
// Returns a fresh pointer-to-struct of the variant's config type.
func mergeConfig(variant VariantSchema, tenantBaseline, policyOverride map[string]any) (any, error) {
	merged := map[string]any{}
	for k, v := range tenantBaseline {
		merged[k] = v
	}
	for k, v := range policyOverride {
		merged[k] = v
	}
	cfg := variant.NewConfig()
	if err := decodeConfig(merged, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// =============================================================================
// EXAMPLE PLUGIN 1: Telemetry — 3 variants, the marquee request-overridable
//                  config carries the actual divert decision
// =============================================================================

type telemetryPlugin struct{}

func (p *telemetryPlugin) Name() string { return "telemetry" }

func (p *telemetryPlugin) Variants() []VariantSchema {
	return []VariantSchema{
		{
			Name:      "standard",
			NewConfig: func() any { return &telemetryStandardConfig{} },
			// Endpoint + auth are infrastructure, set at TenantLoader; policy
			// may NOT redirect telemetry to a different OTLP endpoint per
			// request. SampleRate + IncludeBody ARE policy-overridable.
			TenantOnlyKeys: []string{"endpoint", "otlp_token", "region"},
		},
		{
			Name:      "s3-divert",
			NewConfig: func() any { return &telemetryS3DivertConfig{} },
			// Region is tenant infra; Bucket + KeyPrefix are deliberately
			// overridable so a red-team rule can specify the protected bucket.
			TenantOnlyKeys: []string{"region", "kms_key_id"},
		},
		{
			Name:      "none",
			NewConfig: func() any { return &telemetryNoneConfig{} },
		},
	}
}

type telemetryStandardConfig struct {
	Endpoint    string  `json:"endpoint"`
	OTLPToken   string  `json:"otlp_token"`
	Region      string  `json:"region"`
	SampleRate  float64 `json:"sample_rate"`
	IncludeBody bool    `json:"include_body"`
}

type telemetryS3DivertConfig struct {
	Bucket    string `json:"bucket"`
	KeyPrefix string `json:"key_prefix"`
	Region    string `json:"region"`
	KMSKeyID  string `json:"kms_key_id"`
}

type telemetryNoneConfig struct{}

func (p *telemetryPlugin) RunVariant(_ context.Context, req *exampleRequest, variant string, cfg any, log *eventLog) error {
	switch variant {
	case "standard":
		c := cfg.(*telemetryStandardConfig)
		bodyHint := ""
		if c.IncludeBody {
			bodyHint = fmt.Sprintf(" prompt=%q", req.Prompt)
		}
		log.Add("TEL[standard] endpoint=%s sample=%.2f%s", c.Endpoint, c.SampleRate, bodyHint)
	case "s3-divert":
		c := cfg.(*telemetryS3DivertConfig)
		log.Add("TEL[s3-divert] s3://%s/%s%s", c.Bucket, c.KeyPrefix, req.RequestID)
	case "none":
		log.Add("TEL[none] (telemetry suppressed)")
	default:
		return fmt.Errorf("telemetry: unknown variant %q", variant)
	}
	return nil
}

// =============================================================================
// EXAMPLE PLUGIN 2: Guardrails — 3 variants, mostly tenant-only config
// =============================================================================

type guardrailsPlugin struct {
	// Simulates a remote scan: returns true if the prompt contains a banned token.
	scanFn func(prompt string, categories []string) (blocked bool, reason string)
}

func (p *guardrailsPlugin) Name() string { return "guardrails" }

func (p *guardrailsPlugin) Variants() []VariantSchema {
	return []VariantSchema{
		{
			Name:      "enforce",
			NewConfig: func() any { return &guardrailsEnforceConfig{} },
			// Scan provider + auth token are infra. Categories + FailOpen are
			// policy-overridable (a per-VK rule might enable extra scans).
			TenantOnlyKeys: []string{"scan_provider", "auth_token"},
		},
		{
			Name:      "audit-only",
			NewConfig: func() any { return &guardrailsAuditOnlyConfig{} },
			TenantOnlyKeys: []string{"scan_provider", "log_sink"},
		},
		{
			Name:      "skip",
			NewConfig: func() any { return &guardrailsSkipConfig{} },
		},
	}
}

type guardrailsEnforceConfig struct {
	ScanProvider string   `json:"scan_provider"`
	AuthToken    string   `json:"auth_token"`
	Categories   []string `json:"categories"` // pii, prompt_injection, jailbreak, etc.
	FailOpen     bool     `json:"fail_open"`
}

type guardrailsAuditOnlyConfig struct {
	ScanProvider string   `json:"scan_provider"`
	LogSink      string   `json:"log_sink"`
	Categories   []string `json:"categories"`
}

type guardrailsSkipConfig struct{}

func (p *guardrailsPlugin) RunVariant(_ context.Context, req *exampleRequest, variant string, cfg any, log *eventLog) error {
	switch variant {
	case "enforce":
		c := cfg.(*guardrailsEnforceConfig)
		blocked, reason := p.scanFn(req.Prompt, c.Categories)
		if blocked {
			log.Add("GUARD[enforce] BLOCKED categories=%v reason=%s", c.Categories, reason)
			return fmt.Errorf("guardrails blocked: %s", reason)
		}
		log.Add("GUARD[enforce] allowed categories=%v", c.Categories)
	case "audit-only":
		c := cfg.(*guardrailsAuditOnlyConfig)
		blocked, reason := p.scanFn(req.Prompt, c.Categories)
		if blocked {
			log.Add("GUARD[audit-only] would-block sink=%s reason=%s", c.LogSink, reason)
		} else {
			log.Add("GUARD[audit-only] clean sink=%s", c.LogSink)
		}
	case "skip":
		log.Add("GUARD[skip] (guardrails bypassed)")
	default:
		return fmt.Errorf("guardrails: unknown variant %q", variant)
	}
	return nil
}

// =============================================================================
// EXAMPLE PLUGIN 3: Audit — sink override is the marquee request-overridable
// =============================================================================

type auditPlugin struct{}

func (p *auditPlugin) Name() string { return "audit" }

func (p *auditPlugin) Variants() []VariantSchema {
	return []VariantSchema{
		{
			Name:      "standard",
			NewConfig: func() any { return &auditStandardConfig{} },
			// Sink is infra. RedactPII + IncludePrompt are policy-overridable.
			TenantOnlyKeys: []string{"sink", "schema_version"},
		},
		{
			Name:      "protected",
			NewConfig: func() any { return &auditProtectedConfig{} },
			TenantOnlyKeys: []string{"encryption_kms_key", "schema_version"},
		},
		{
			Name:      "tamperproof",
			NewConfig: func() any { return &auditTamperproofConfig{} },
			TenantOnlyKeys: []string{"blockchain_anchor", "encryption_kms_key", "schema_version"},
		},
	}
}

type auditStandardConfig struct {
	Sink          string `json:"sink"`
	SchemaVersion string `json:"schema_version"`
	RedactPII     bool   `json:"redact_pii"`
	IncludePrompt bool   `json:"include_prompt"`
	RetentionDays int    `json:"retention_days"`
}

type auditProtectedConfig struct {
	// Note: no Sink tenant-only key here; the policy CHOOSES the protected
	// sink because that's the whole point of the variant — divert to a
	// security-team-owned bucket per request.
	Sink             string `json:"sink"`
	SchemaVersion    string `json:"schema_version"`
	EncryptionKMSKey string `json:"encryption_kms_key"`
	RedactPII        bool   `json:"redact_pii"`
	TamperEvidence   bool   `json:"tamper_evidence"`
}

type auditTamperproofConfig struct {
	BlockchainAnchor string `json:"blockchain_anchor"`
	EncryptionKMSKey string `json:"encryption_kms_key"`
	SchemaVersion    string `json:"schema_version"`
}

func (p *auditPlugin) RunVariant(_ context.Context, req *exampleRequest, variant string, cfg any, log *eventLog) error {
	switch variant {
	case "standard":
		c := cfg.(*auditStandardConfig)
		body := req.Prompt
		if c.RedactPII {
			body = "[REDACTED]"
		}
		log.Add("AUD[standard] sink=%s pii_redacted=%v body=%q", c.Sink, c.RedactPII, body)
	case "protected":
		c := cfg.(*auditProtectedConfig)
		log.Add("AUD[protected] sink=%s kms=%s tamper_evidence=%v", c.Sink, c.EncryptionKMSKey, c.TamperEvidence)
	case "tamperproof":
		c := cfg.(*auditTamperproofConfig)
		log.Add("AUD[tamperproof] anchor=%s kms=%s", c.BlockchainAnchor, c.EncryptionKMSKey)
	default:
		return fmt.Errorf("audit: unknown variant %q", variant)
	}
	return nil
}

// =============================================================================
// Tenant baseline + execution harness
// =============================================================================

// tenantBaselineConfigs is what the TenantLoader would set per
// (plugin, variant). Real production wiring lives in the loader; this
// stand-in keeps the test self-contained.
type tenantBaselineConfigs map[string]map[string]map[string]any // [plugin][variant] -> config

func executePlanWithMerge(ctx context.Context, req *exampleRequest, plan ChainPlan, registry map[string]ExamplePlugin, baselines tenantBaselineConfigs, log *eventLog) error {
	for _, step := range plan.Steps {
		p, ok := registry[step.Plugin]
		if !ok {
			return fmt.Errorf("execute: unknown plugin %q", step.Plugin)
		}
		variant, ok := findVariant(p.Variants(), step.Variant)
		if !ok {
			return fmt.Errorf("execute: %s has no variant %q", step.Plugin, step.Variant)
		}
		baseline := baselines[step.Plugin][step.Variant]
		cfg, err := mergeConfig(variant, baseline, step.Config)
		if err != nil {
			return fmt.Errorf("execute %s/%s: %w", step.Plugin, step.Variant, err)
		}
		if err := p.RunVariant(ctx, req, step.Variant, cfg, log); err != nil && !errors.Is(err, errExpectedBlock) {
			return err
		}
	}
	return nil
}

var errExpectedBlock = errors.New("expected block (test fixture)")

// =============================================================================
// SCENARIO 1: Red-team VK — divert telemetry to protected S3, bypass
//             guardrails, route audit to security-owned sink
// =============================================================================

func TestExamples_RedTeamDivertEndToEnd(t *testing.T) {
	registry := map[string]ExamplePlugin{
		"telemetry":  &telemetryPlugin{},
		"guardrails": &guardrailsPlugin{scanFn: func(p string, _ []string) (bool, string) {
			if strings.Contains(p, "FORBIDDEN") {
				return true, "banned token"
			}
			return false, ""
		}},
		"audit": &auditPlugin{},
	}

	// What the TenantLoader has resolved for "acme" tenant at Acquire time.
	baselines := tenantBaselineConfigs{
		"telemetry": {
			"standard": {
				"endpoint":     "https://otel.acme.internal/v1/traces",
				"otlp_token":   "fake-tenant-token",
				"region":       "us-east-1",
				"sample_rate":  1.0,
				"include_body": false,
			},
			"s3-divert": {
				"region":     "us-east-1",
				"kms_key_id": "alias/acme-tenant-default",
				"bucket":     "acme-divert-default", // baseline default; policy will override per request
			},
		},
		"guardrails": {
			"enforce": {
				"scan_provider": "https://guardrails.acme.internal/scan",
				"auth_token":    "fake-scan-token",
				"categories":    []any{"pii", "prompt_injection"},
				"fail_open":     true,
			},
			"audit-only": {
				"scan_provider": "https://guardrails.acme.internal/scan",
				"log_sink":      "s3://acme-guardrails-audit/",
				"categories":    []any{"pii", "prompt_injection"},
			},
			"skip": {},
		},
		"audit": {
			"standard": {
				"sink":           "postgres://audit-db.acme.internal/standard",
				"schema_version": "v3",
				"redact_pii":     false,
				"include_prompt": true,
				"retention_days": 30,
			},
			"protected": {
				"encryption_kms_key": "alias/acme-protected-audit",
				"schema_version":     "v3",
				// Sink intentionally omitted from baseline — protected variant
				// expects the *policy* to choose the per-request destination.
			},
		},
	}

	// Policy: one rule for red-team VKs. The Config maps on each step are
	// the per-request OVERRIDES on top of the tenant baselines above.
	rules := []Rule{
		{
			Name: "red-team-divert",
			Expr: `"red-team" in vk_tags`,
			Plan: []ChainStep{
				{Plugin: "guardrails", Variant: "skip"},
				{Plugin: "telemetry", Variant: "s3-divert", Config: map[string]any{
					"bucket":     "acme-redteam-audit", // OVERRIDES baseline's "acme-divert-default"
					"key_prefix": "scans/",
				}},
				{Plugin: "audit", Variant: "protected", Config: map[string]any{
					"sink":            "s3://acme-redteam-evidence/",
					"redact_pii":      true,
					"tamper_evidence": true,
				}},
			},
		},
	}
	basePlan := []ChainStep{
		{Plugin: "guardrails", Variant: "enforce"},
		{Plugin: "telemetry", Variant: "standard"},
		{Plugin: "audit", Variant: "standard"},
	}

	// Policy-load-time validation: catches typos + tenant-only-override
	// violations BEFORE any request runs.
	require.NoError(t, ValidatePolicyAgainstPlugins(rules, basePlan, registry))

	policy, err := NewCELChainPolicy(rules, basePlan)
	require.NoError(t, err)

	// Red-team request
	{
		log := &eventLog{}
		req := &exampleRequest{
			RequestID: "r-rt-001",
			TenantID:  "acme",
			VKID:      "vk-red-001",
			Prompt:    "what's the password for FORBIDDEN-system?",
		}
		plan, err := policy.Select(context.Background(), PolicyInput{
			TenantID: "acme",
			VK:       VKSnapshot{ID: "vk-red-001", Tags: []string{"red-team", "internal"}},
		})
		require.NoError(t, err)
		require.NoError(t, executePlanWithMerge(context.Background(), req, plan, registry, baselines, &eventLog{}))

		// Re-run to capture events (keeping the call shape compact).
		log = &eventLog{}
		require.NoError(t, executePlanWithMerge(context.Background(), req, plan, registry, baselines, log))

		require.Equal(t, []string{
			"GUARD[skip] (guardrails bypassed)",
			"TEL[s3-divert] s3://acme-redteam-audit/scans/r-rt-001",
			"AUD[protected] sink=s3://acme-redteam-evidence/ kms=alias/acme-protected-audit tamper_evidence=true",
		}, log.events)

		// The audit trail is visible — every step says WHY it ran.
		require.Contains(t, plan.Why[0], "matched rule: red-team-divert")
		for _, s := range plan.Steps {
			require.Equal(t, "red-team-divert", s.Why)
		}
	}

	// Standard prod VK — same registry, same baselines, totally different
	// execution because the policy fell through to the base plan.
	{
		log := &eventLog{}
		req := &exampleRequest{
			RequestID: "r-std-001",
			TenantID:  "acme",
			VKID:      "vk-std-001",
			Prompt:    "summarize this customer review",
		}
		plan, err := policy.Select(context.Background(), PolicyInput{
			TenantID: "acme",
			VK:       VKSnapshot{ID: "vk-std-001", Tags: []string{"prod"}},
		})
		require.NoError(t, err)
		require.NoError(t, executePlanWithMerge(context.Background(), req, plan, registry, baselines, log))

		require.Equal(t, []string{
			"GUARD[enforce] allowed categories=[pii prompt_injection]",
			"TEL[standard] endpoint=https://otel.acme.internal/v1/traces sample=1.00",
			`AUD[standard] sink=postgres://audit-db.acme.internal/standard pii_redacted=false body="summarize this customer review"`,
		}, log.events)
		require.Contains(t, plan.Why[0], "no rule matched")
	}
}

// =============================================================================
// SCENARIO 2: Low-trust customer — extra guardrails categories + redacted
//             audit + lower telemetry sample rate
// =============================================================================

func TestExamples_LowTrustCustomerOverridesKnobs(t *testing.T) {
	registry := map[string]ExamplePlugin{
		"telemetry":  &telemetryPlugin{},
		"guardrails": &guardrailsPlugin{scanFn: func(_ string, _ []string) (bool, string) { return false, "" }},
		"audit":      &auditPlugin{},
	}
	baselines := tenantBaselineConfigs{
		"telemetry": {"standard": {
			"endpoint": "https://otel.acme.internal/v1/traces", "otlp_token": "t", "region": "us-east-1",
			"sample_rate": 1.0, "include_body": false,
		}},
		"guardrails": {"enforce": {
			"scan_provider": "https://scan.internal", "auth_token": "t",
			"categories": []any{"pii"}, "fail_open": true,
		}},
		"audit": {"standard": {
			"sink": "postgres://audit/main", "schema_version": "v3",
			"redact_pii": false, "include_prompt": true, "retention_days": 30,
		}},
	}
	rules := []Rule{
		{
			Name: "low-trust-customer",
			Expr: `vk_role == "low-trust"`,
			Plan: []ChainStep{
				{Plugin: "guardrails", Variant: "enforce", Config: map[string]any{
					// Add prompt-injection + jailbreak to the categories list.
					"categories": []any{"pii", "prompt_injection", "jailbreak"},
					"fail_open":  false, // hardened: closed-on-failure
				}},
				{Plugin: "telemetry", Variant: "standard", Config: map[string]any{
					"sample_rate":  0.10, // sample only 10% to manage cost on noisy tenant
					"include_body": true, // include body for compliance
				}},
				{Plugin: "audit", Variant: "standard", Config: map[string]any{
					"redact_pii":     true, // hardened: always redact for low-trust
					"include_prompt": true,
					"retention_days": 365,
				}},
			},
		},
	}
	basePlan := []ChainStep{
		{Plugin: "guardrails", Variant: "enforce"},
		{Plugin: "telemetry", Variant: "standard"},
		{Plugin: "audit", Variant: "standard"},
	}
	require.NoError(t, ValidatePolicyAgainstPlugins(rules, basePlan, registry))
	policy, err := NewCELChainPolicy(rules, basePlan)
	require.NoError(t, err)

	log := &eventLog{}
	req := &exampleRequest{RequestID: "r-lt-001", TenantID: "acme", VKID: "vk-lt-001", Prompt: "PII: my email is x@y"}
	plan, err := policy.Select(context.Background(), PolicyInput{
		TenantID: "acme",
		VK:       VKSnapshot{ID: "vk-lt-001", Role: "low-trust"},
	})
	require.NoError(t, err)
	require.NoError(t, executePlanWithMerge(context.Background(), req, plan, registry, baselines, log))

	// All three steps execute the SAME variant as the base plan, but the
	// overrides land cleanly: extra categories, lower sample rate, PII
	// redacted in audit body.
	require.Equal(t, []string{
		"GUARD[enforce] allowed categories=[pii prompt_injection jailbreak]",
		`TEL[standard] endpoint=https://otel.acme.internal/v1/traces sample=0.10 prompt="PII: my email is x@y"`,
		`AUD[standard] sink=postgres://audit/main pii_redacted=true body="[REDACTED]"`,
	}, log.events)
}

// =============================================================================
// SCENARIO 3: Validator catches typos + tenant-only-key violations at
//             policy-LOAD time (not at request time)
// =============================================================================

func TestExamples_ValidatorRejectsBadPolicies(t *testing.T) {
	registry := map[string]ExamplePlugin{
		"telemetry":  &telemetryPlugin{},
		"guardrails": &guardrailsPlugin{},
		"audit":      &auditPlugin{},
	}
	basePlan := []ChainStep{{Plugin: "guardrails", Variant: "enforce"}}

	cases := []struct {
		name      string
		rule      Rule
		wantInErr string
	}{
		{
			name: "unknown variant",
			rule: Rule{Name: "r", Expr: `true`, Plan: []ChainStep{
				{Plugin: "telemetry", Variant: "smoke-signals"},
			}},
			wantInErr: `no variant "smoke-signals"`,
		},
		{
			name: "unknown plugin",
			rule: Rule{Name: "r", Expr: `true`, Plan: []ChainStep{
				{Plugin: "carrier-pigeon", Variant: "standard"},
			}},
			wantInErr: `unknown plugin "carrier-pigeon"`,
		},
		{
			name: "unknown config key — typo'd field",
			rule: Rule{Name: "r", Expr: `true`, Plan: []ChainStep{
				{Plugin: "telemetry", Variant: "standard", Config: map[string]any{
					"sampel_rate": 0.5, // typo: should be sample_rate
				}},
			}},
			wantInErr: "sampel_rate",
		},
		{
			name: "policy tries to override tenant-only key",
			rule: Rule{Name: "r", Expr: `true`, Plan: []ChainStep{
				{Plugin: "telemetry", Variant: "standard", Config: map[string]any{
					"endpoint": "https://attacker.example/v1/traces", // would redirect telemetry per-request
				}},
			}},
			wantInErr: "tenant-only key",
		},
		{
			name: "wrong type for config value",
			rule: Rule{Name: "r", Expr: `true`, Plan: []ChainStep{
				{Plugin: "telemetry", Variant: "standard", Config: map[string]any{
					"sample_rate": "all of it please", // not a number
				}},
			}},
			wantInErr: "sample_rate",
		},
	}

	// Sort cases deterministically (subtests run in slice order anyway, but
	// this makes the test output stable when someone reorders).
	sort.Slice(cases, func(i, j int) bool { return cases[i].name < cases[j].name })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePolicyAgainstPlugins([]Rule{tc.rule}, basePlan, registry)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantInErr)
		})
	}
}
