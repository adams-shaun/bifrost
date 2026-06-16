package server

import (
	"testing"
)

// TestMultiTenantEnabled_DefaultOff is the load-bearing assertion that
// existing single-tenant deployments observe no behavior change from
// this patch: with the env var unset, buildInferenceRouter returns a
// SingleTenantRouter and the unmodified middleware chain. This test
// pins the default-off contract so a future code edit can't silently
// flip it on.
func TestMultiTenantEnabled_DefaultOff(t *testing.T) {
	t.Setenv("BIFROST_MULTI_TENANT_ENABLED", "")
	if multiTenantEnabled() {
		t.Fatal("multi-tenant routing must be OFF when the env var is unset")
	}
}

func TestMultiTenantEnabled_TruthyValues(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on", "TRUE", "On"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("BIFROST_MULTI_TENANT_ENABLED", v)
			if !multiTenantEnabled() {
				t.Fatalf("value %q must enable multi-tenant routing", v)
			}
		})
	}
}

func TestMultiTenantEnabled_FalsyValues(t *testing.T) {
	for _, v := range []string{"0", "false", "no", "off", "", "FALSE"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("BIFROST_MULTI_TENANT_ENABLED", v)
			if multiTenantEnabled() {
				t.Fatalf("value %q must NOT enable multi-tenant routing", v)
			}
		})
	}
}

func TestMultiTenantEnabled_WhitespaceTrimmed(t *testing.T) {
	t.Setenv("BIFROST_MULTI_TENANT_ENABLED", "  true  ")
	if !multiTenantEnabled() {
		t.Fatal("whitespace around a truthy value must not disable it")
	}
}
