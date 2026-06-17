package telemetry

import (
	"bytes"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// noopLogger is the minimum schemas.Logger for tests that don't care
// about output.
type noopLogger struct{}

func (noopLogger) Debug(string, ...any)                   {}
func (noopLogger) Info(string, ...any)                    {}
func (noopLogger) Warn(string, ...any)                    {}
func (noopLogger) Error(string, ...any)                   {}
func (noopLogger) Fatal(string, ...any)                   {}
func (noopLogger) SetLevel(schemas.LogLevel)              {}
func (noopLogger) SetOutputType(schemas.LoggerOutputType) {}
func (noopLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

// defaultLabelOrder mirrors the positional order of defaultBifrostLabels
// in main.go so a labels MAP can be expanded into WithLabelValues' VARIADIC
// argument list deterministically. Keep in sync with main.go ~line 178.
var defaultLabelOrder = []string{
	"provider", "model", "alias", "method", "tenant_id",
	"virtual_key_id", "virtual_key_name", "routing_engine_used",
	"routing_rule_id", "routing_rule_name", "selected_key_id",
	"selected_key_name", "fallback_index", "team_id", "team_name",
	"customer_id", "customer_name",
}

func labelsToPositional(m map[string]string) []string {
	out := make([]string, len(defaultLabelOrder))
	for i, k := range defaultLabelOrder {
		out[i] = m[k] // map zero value (empty string) is what Prom wants for absent
	}
	return out
}

// emitAll fires the five marquee counter metrics with the given label
// set — no policy applied. This is "today's behavior."
func emitAll(plugin *PrometheusPlugin, labels map[string]string) {
	positional := labelsToPositional(labels)
	plugin.UpstreamRequestsTotal.WithLabelValues(positional...).Inc()
	plugin.SuccessRequestsTotal.WithLabelValues(positional...).Inc()
	plugin.InputTokensTotal.WithLabelValues(positional...).Add(427)
	plugin.OutputTokensTotal.WithLabelValues(positional...).Add(89)
	plugin.CostTotal.WithLabelValues(positional...).Add(0.00213)
}

// emitWithPolicy is the proposed wiring: every emission funnels through
// policy.Decide, which short-circuits when the metric is denied, when
// the sample-rate drop fires, or rewrites the label set when deny labels
// are configured.
func emitWithPolicy(plugin *PrometheusPlugin, policy *TelemetryPolicy, labels map[string]string) {
	tid := labels["tenant_id"]
	cases := []struct {
		name string
		fire func(positional []string)
	}{
		{"bifrost_upstream_requests_total", func(p []string) { plugin.UpstreamRequestsTotal.WithLabelValues(p...).Inc() }},
		{"bifrost_success_requests_total", func(p []string) { plugin.SuccessRequestsTotal.WithLabelValues(p...).Inc() }},
		{"bifrost_input_tokens_total", func(p []string) { plugin.InputTokensTotal.WithLabelValues(p...).Add(427) }},
		{"bifrost_output_tokens_total", func(p []string) { plugin.OutputTokensTotal.WithLabelValues(p...).Add(89) }},
		{"bifrost_cost_total", func(p []string) { plugin.CostTotal.WithLabelValues(p...).Add(0.00213) }},
	}
	for _, c := range cases {
		emit, lbls := policy.Decide(c.name, tid, labels)
		if !emit {
			continue
		}
		c.fire(labelsToPositional(lbls))
	}
}

// dumpRegistry encodes a registry to Prometheus text exposition format,
// filtered to the named metric families so the output stays focused on
// what we just emitted.
func dumpRegistry(t *testing.T, registry *prometheus.Registry, names ...string) string {
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]struct{}{}
	for _, n := range names {
		want[n] = struct{}{}
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; !ok {
			continue
		}
		if err := enc.Encode(mf); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	return buf.String()
}

// sampleRequests is the synthetic workload used by both the unfiltered
// and filtered dumps so the before/after is a direct comparison.
var sampleRequests = []map[string]string{
	{
		"provider": "openai", "model": "gpt-4o-mini", "method": "chat_completion",
		"tenant_id": "acme", "virtual_key_id": "vk-acme-001", "virtual_key_name": "prod-acme",
		"selected_key_id": "key-acme-1", "selected_key_name": "primary", "fallback_index": "0",
		"team_id": "team-eng", "team_name": "Engineering",
		"customer_id": "cust-acme", "customer_name": "Acme Corp",
	},
	{
		"provider": "openai", "model": "gpt-4o", "method": "chat_completion",
		"tenant_id": "acme", "virtual_key_id": "vk-acme-001", "virtual_key_name": "prod-acme",
		"selected_key_id": "key-acme-1", "selected_key_name": "primary", "fallback_index": "0",
		"team_id": "team-eng", "team_name": "Engineering",
		"customer_id": "cust-acme", "customer_name": "Acme Corp",
	},
	{
		"provider": "anthropic", "model": "claude-3-haiku", "method": "chat_completion",
		"tenant_id": "globex", "virtual_key_id": "vk-globex-001", "virtual_key_name": "prod-globex",
		"selected_key_id": "key-globex-1", "selected_key_name": "primary", "fallback_index": "0",
		"team_id": "team-eng", "team_name": "Engineering",
		"customer_id": "cust-globex", "customer_name": "Globex Inc.",
	},
	{
		"provider": "anthropic", "model": "claude-3-haiku", "method": "chat_completion",
		"tenant_id": "globex", "virtual_key_id": "vk-globex-001", "virtual_key_name": "prod-globex",
		"selected_key_id": "key-globex-1", "selected_key_name": "primary", "fallback_index": "0",
		"team_id": "team-eng", "team_name": "Engineering",
		"customer_id": "cust-globex", "customer_name": "Globex Inc.",
	},
}

var dumpMetricNames = []string{
	"bifrost_upstream_requests_total",
	"bifrost_success_requests_total",
	"bifrost_input_tokens_total",
	"bifrost_output_tokens_total",
	"bifrost_cost_total",
}

// TestExpositionExample_Unfiltered produces a real Prometheus-format
// dump of what the plugin emits TODAY (no tenant filters applied).
// Goal: give an unambiguous, copy-pasteable picture of the label
// cardinality + shape so the design conversation in
// docs/multi-tenant-f5/09-telemetry-tenant-config.md has something
// concrete to point at.
//
// Use `go test -run TestExpositionExample_Unfiltered -v ./plugins/telemetry/`
// to see the dump. The assertions just guarantee output exists; reading
// the t.Logf output IS the test.
func TestExpositionExample_Unfiltered(t *testing.T) {
	registry := prometheus.NewRegistry()
	plugin, err := Init(&Config{Registry: registry}, nil, noopLogger{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer plugin.Cleanup()
	for _, req := range sampleRequests {
		emitAll(plugin, req)
	}
	out := dumpRegistry(t, registry, dumpMetricNames...)
	if out == "" {
		t.Fatalf("no exposition emitted")
	}
	t.Logf("\n========== UNFILTERED EXPOSITION (today's behavior) ==========\n%s\n=============================================================", out)
}

// TestExpositionExample_SideBySide is the headline demo: shows the SAME
// four synthetic requests rendered (a) today's way, no policy, and
// (b) with a TenantTelemetryConfig applied where tenant "globex" has:
//   - LabelDenyList = [customer_id, customer_name]   (hide PII)
//   - MetricDenyList = [bifrost_input_tokens_total,
//                       bifrost_output_tokens_total] (compliance opt-out)
//   - SampleRate = nil (always emit — make the dump deterministic so
//                       the demo is reproducible)
//
// Tenant "acme" has no config — emits unchanged. The dump makes the
// before/after concrete in a way the prose in doc 09 can't.
func TestExpositionExample_SideBySide(t *testing.T) {
	// (a) Unfiltered — today.
	unfilteredRegistry := prometheus.NewRegistry()
	unfilteredPlugin, err := Init(&Config{Registry: unfilteredRegistry}, nil, noopLogger{})
	if err != nil {
		t.Fatalf("Init unfiltered: %v", err)
	}
	defer unfilteredPlugin.Cleanup()
	for _, req := range sampleRequests {
		emitAll(unfilteredPlugin, req)
	}
	unfilteredOut := dumpRegistry(t, unfilteredRegistry, dumpMetricNames...)

	// (b) Filtered — same workload, policy applied for tenant "globex".
	filteredRegistry := prometheus.NewRegistry()
	filteredPlugin, err := Init(&Config{Registry: filteredRegistry}, nil, noopLogger{})
	if err != nil {
		t.Fatalf("Init filtered: %v", err)
	}
	defer filteredPlugin.Cleanup()

	store := newInMemoryTenantConfigStore()
	store.Set("globex", &TenantTelemetryConfig{
		MetricDenyList: []string{"bifrost_input_tokens_total", "bifrost_output_tokens_total"},
		LabelDenyList:  []string{"customer_id", "customer_name"},
	})
	policy := NewTelemetryPolicy(store, func() float64 { return 0.0 })

	for _, req := range sampleRequests {
		emitWithPolicy(filteredPlugin, policy, req)
	}
	filteredOut := dumpRegistry(t, filteredRegistry, dumpMetricNames...)

	if unfilteredOut == "" || filteredOut == "" {
		t.Fatalf("expected output in both runs (unfiltered=%dB filtered=%dB)", len(unfilteredOut), len(filteredOut))
	}

	t.Logf("\n========== UNFILTERED (today's behavior) ==========\n%s\n", unfilteredOut)
	t.Logf("\n========== FILTERED (policy: globex denies customer_id/customer_name + token-count metrics) ==========\n%s\n=========================================================================", filteredOut)
}
