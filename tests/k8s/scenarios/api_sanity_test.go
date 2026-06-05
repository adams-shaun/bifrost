//go:build k8s

package scenarios

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/tests/k8s/fixtures"
	"gopkg.volterra.us/go-bifrost-ai/types"
)

// TestAPISanity stands up the full stack — postgres + mock-llm (helm umbrella
// chart) and bifrost (serelib-rendered, kubectl-applied) — on a kind cluster,
// then creates exactly ONE of each governance API entity through the
// go-bifrost-ai SDK, seeding FK/related data in dependency order. Every create
// must succeed and return a non-empty ID.
//
// Run with:
//
//	make image   # builds bifrost:local-test from the F5XC-patched tree
//	cd tests/k8s && GOWORK=off BIFROST_K8S_SKIP_BUILD=1 \
//	    go test -tags=k8s -count=1 -timeout 30m -run '^TestAPISanity$' -v ./scenarios/...
//
// Switches: BIFROST_K8S_KEEP=1 (skip teardown for inspection),
// BIFROST_K8S_LOG_PODS_ALWAYS=1 (dump bifrost pod logs even on success).
func TestAPISanity(t *testing.T) {
	c, stopCluster := fixtures.NewKindCluster(t, fixtures.WithKindReuse())
	defer stopCluster()

	bf, teardown := fixtures.NewBifrostInstall(t, c)
	defer teardown()

	// Provider + key (raw API): prerequisite for the VirtualKey, and itself part
	// of the API surface we sanity-check.
	bf.ConfigureMockProvider(t)

	sdk := bf.AdminClient()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ok := func(kind string, err error, id string) string {
		t.Helper()
		if err != nil {
			t.Fatalf("create %s: %v", kind, err)
		}
		if id == "" {
			t.Fatalf("create %s: empty id", kind)
		}
		t.Logf("created %s id=%s", kind, id)
		return id
	}

	// 1. Customer — root entity, no dependencies.
	cust, err := sdk.Governance.Customers.Create(ctx, &types.CreateCustomerRequest{
		Name: "sanity-customer",
	})
	custID := ok("customer", err, customerID(cust))

	// 2. Team — FK: customer.
	team, err := sdk.Governance.Teams.Create(ctx, &types.CreateTeamRequest{
		Name:       "sanity-team",
		CustomerID: &custID,
	})
	tID := ok("team", err, teamID(team))

	// 3. User — FK: team; embedded budget.
	user, err := sdk.Governance.Users.Create(ctx, &types.CreateUserRequest{
		Name:   "sanity-user",
		Email:  "sanity@example.test",
		TeamID: &tID,
		Budget: &types.CreateBudgetRequest{MaxLimit: 1000, ResetDuration: "1h"},
	})
	ok("user", err, userID(user))

	// 4. VirtualKey — FK: team (a VK attaches to exactly one of team/customer/
	//    access-profile); references the openai provider; embedded rate limit.
	vk, err := sdk.Governance.VirtualKeys.Create(ctx, &types.CreateVirtualKeyRequest{
		Name:   "sanity-vk",
		TeamID: &tID,
		ProviderConfigs: []types.CreateVKProviderConfig{{
			Provider:      "openai",
			AllowedModels: schemas.WhiteList{"*"},
			KeyIDs:        schemas.WhiteList{"*"},
		}},
		RateLimit: &types.CreateRateLimitRequest{
			TokenMaxLimit:      ptrI64(1000000),
			TokenResetDuration: ptrStr("1h"),
		},
	})
	vkID := ok("virtual_key", err, vkIDValue(vk))

	// 5. RoutingRule — targets reference provider/model by name.
	rr, err := sdk.Governance.RoutingRules.Create(ctx, &types.CreateRoutingRuleRequest{
		Name:          "sanity-rule",
		CelExpression: `request_type == "chat_completion"`,
		Targets: []types.RoutingTarget{{
			Provider: ptrStr("openai"),
			Model:    ptrStr("gpt-4o-mock"),
			Weight:   1.0,
		}},
	})
	ok("routing_rule", err, routingRuleID(rr))

	// 6. ModelConfig — embedded budget.
	mc, err := sdk.Governance.ModelConfigs.Create(ctx, &types.CreateModelConfigRequest{
		ModelName: "gpt-4o-mock",
		Provider:  ptrStr("openai"),
		Budget:    &types.CreateBudgetRequest{MaxLimit: 500, ResetDuration: "24h"},
	})
	ok("model_config", err, modelConfigID(mc))

	// 7. PricingOverride — FK: virtual key.
	po, err := sdk.Governance.PricingOverrides.Create(ctx, &types.CreatePricingOverrideRequest{
		Name:         "sanity-pricing",
		ScopeKind:    types.ScopeKindVirtualKey,
		VirtualKeyID: &vkID,
		MatchType:    types.MatchTypeExact,
		Pattern:      "gpt-4o-mock",
		RequestTypes: []schemas.RequestType{schemas.ChatCompletionRequest},
		Patch: types.PricingOptions{
			InputCostPerToken:  ptrF64(0.001),
			OutputCostPerToken: ptrF64(0.002),
		},
	})
	ok("pricing_override", err, pricingOverrideID(po))

	t.Logf("API sanity OK: provider+key, customer, team, user, virtual_key, routing_rule, model_config, pricing_override")
}

// Response-ID accessors are nil-safe so an errored (nil) response doesn't panic
// before the error is reported by ok().
func customerID(r *types.CustomerResponse) string {
	if r == nil {
		return ""
	}
	return r.Customer.ID
}
func teamID(r *types.TeamResponse) string {
	if r == nil {
		return ""
	}
	return r.Team.ID
}
func userID(r *types.UserResponse) string {
	if r == nil {
		return ""
	}
	return r.User.ID
}
func vkIDValue(r *types.VirtualKeyResponse) string {
	if r == nil {
		return ""
	}
	return r.VirtualKey.ID
}
func routingRuleID(r *types.RoutingRuleResponse) string {
	if r == nil {
		return ""
	}
	return r.Rule.ID
}
func modelConfigID(r *types.ModelConfigResponse) string {
	if r == nil {
		return ""
	}
	return r.ModelConfig.ID
}
func pricingOverrideID(r *types.PricingOverrideResponse) string {
	if r == nil {
		return ""
	}
	return r.PricingOverride.ID
}

func ptrStr(s string) *string   { return &s }
func ptrI64(v int64) *int64     { return &v }
func ptrF64(f float64) *float64 { return &f }
