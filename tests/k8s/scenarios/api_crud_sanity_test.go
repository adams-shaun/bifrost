//go:build k8s

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/tests/k8s/fixtures"
	bifrostai "gopkg.volterra.us/go-bifrost-ai"
	"gopkg.volterra.us/go-bifrost-ai/types"
)

// TestAPICRUDSanity drives the go-bifrost-ai SDK through a create / read / list
// / update / delete lifecycle for every resource that exposes one. Each call is
// classified PASS / UNIMPLEMENTED (404·405·501) / FAIL; the suite fails only on
// FAIL, and prints a per-method coverage matrix at the end. This both smoke-
// tests the SDK<->server contract and reports which CRUD endpoints the patched
// server actually implements.
//
// Run: cd tests/k8s && GOWORK=off BIFROST_K8S_SKIP_BUILD=1 \
//        go test -tags=k8s -count=1 -timeout 30m -run '^TestAPICRUDSanity$' -v ./scenarios/...
func TestAPICRUDSanity(t *testing.T) {
	c, stopCluster := fixtures.NewKindCluster(t, fixtures.WithKindReuse())
	defer stopCluster()

	bf, teardown := fixtures.NewBifrostInstall(t, c)
	defer teardown()

	// Ensures the "openai" provider + key exist (VKs and provider-governance
	// reference them).
	bf.ConfigureMockProvider(t)

	sdk := bf.AdminClient()
	rec := &recorder{}
	defer rec.report(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// ---- Provider (+ keys) -------------------------------------------------
	// Use a provider distinct from the mock "openai" so we don't clobber it.
	t.Run("provider", func(t *testing.T) {
		const prov = schemas.Anthropic
		_, err := sdk.Providers.Create(ctx, &types.CreateProviderRequest{
			Provider: prov,
			NetworkConfig: &schemas.NetworkConfig{
				BaseURL:                        bf.MockLLM.InClusterURL,
				DefaultRequestTimeoutInSeconds: 30,
			},
			ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: 10, BufferSize: 50},
		})
		if !rec.step(t, "Providers.Create", err) {
			return
		}
		t.Cleanup(func() { _ = sdk.Providers.Delete(context.Background(), prov) })

		_, err = sdk.Providers.Get(ctx, prov)
		rec.step(t, "Providers.Get", err)
		_, err = sdk.Providers.List(ctx)
		rec.step(t, "Providers.List", err)
		_, err = sdk.Providers.Update(ctx, prov, &types.UpdateProviderRequest{
			NetworkConfig:            schemas.NetworkConfig{BaseURL: bf.MockLLM.InClusterURL, DefaultRequestTimeoutInSeconds: 45},
			ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 20, BufferSize: 60},
		})
		rec.step(t, "Providers.Update", err)

		key, err := sdk.Providers.CreateKey(ctx, prov, &schemas.Key{
			Name:   "crud-key",
			Value:  schemas.EnvVar{Val: "sk-crud"},
			Models: schemas.WhiteList{"*"},
			Weight: 1,
		})
		if rec.step(t, "Providers.CreateKey", err) && key != nil {
			_, err = sdk.Providers.GetKey(ctx, prov, key.ID)
			rec.step(t, "Providers.GetKey", err)
			_, err = sdk.Providers.UpdateKey(ctx, prov, key.ID, &schemas.Key{
				Name: "crud-key", Value: schemas.EnvVar{Val: "sk-crud-2"}, Models: schemas.WhiteList{"*"}, Weight: 2,
			})
			rec.step(t, "Providers.UpdateKey", err)
			_, err = sdk.Providers.ListKeys(ctx, prov)
			rec.step(t, "Providers.ListKeys", err)
			rec.step(t, "Providers.DeleteKey", sdk.Providers.DeleteKey(ctx, prov, key.ID))
		}
		rec.step(t, "Providers.Delete", sdk.Providers.Delete(ctx, prov))
	})

	// ---- Governance: Customer ---------------------------------------------
	var custID string
	t.Run("customer", func(t *testing.T) {
		r, err := sdk.Governance.Customers.Create(ctx, &types.CreateCustomerRequest{Name: "crud-customer"})
		if !rec.step(t, "Customers.Create", err) {
			return
		}
		custID = r.Customer.ID
		// NOTE: no subtest cleanup — customer is an FK parent referenced by later
		// subtests; it's deleted inline at the end of the test body.
		_, err = sdk.Governance.Customers.Get(ctx, custID)
		rec.step(t, "Customers.Get", err)
		_, err = sdk.Governance.Customers.List(ctx, nil)
		rec.step(t, "Customers.List", err)
		_, err = sdk.Governance.Customers.Update(ctx, custID, &types.UpdateCustomerRequest{Name: ptrStr("crud-customer-2")})
		rec.step(t, "Customers.Update", err)
		// Delete is exercised at the end so dependent entities can reference it.
	})

	// ---- Governance: Team (-> customer) -----------------------------------
	var teamID string
	t.Run("team", func(t *testing.T) {
		req := &types.CreateTeamRequest{Name: "crud-team"}
		if custID != "" {
			req.CustomerID = &custID
		}
		r, err := sdk.Governance.Teams.Create(ctx, req)
		if !rec.step(t, "Teams.Create", err) {
			return
		}
		teamID = r.Team.ID
		// FK parent (referenced by user/vk subtests) — deleted inline at body end.
		_, err = sdk.Governance.Teams.Get(ctx, teamID)
		rec.step(t, "Teams.Get", err)
		_, err = sdk.Governance.Teams.List(ctx, nil)
		rec.step(t, "Teams.List", err)
		_, err = sdk.Governance.Teams.Update(ctx, teamID, &types.UpdateTeamRequest{Name: ptrStr("crud-team-2")})
		rec.step(t, "Teams.Update", err)
	})

	// ---- Governance: User (-> team) ---------------------------------------
	t.Run("user", func(t *testing.T) {
		req := &types.CreateUserRequest{Name: "crud-user", Email: "crud@example.test"}
		if teamID != "" {
			req.TeamID = &teamID
		}
		r, err := sdk.Governance.Users.Create(ctx, req)
		if !rec.step(t, "Users.Create", err) {
			return
		}
		id := r.User.ID
		t.Cleanup(func() { _ = sdk.Governance.Users.Delete(context.Background(), id) })
		_, err = sdk.Governance.Users.Get(ctx, id)
		rec.step(t, "Users.Get", err)
		_, err = sdk.Governance.Users.List(ctx, nil)
		rec.step(t, "Users.List", err)
		_, err = sdk.Governance.Users.Update(ctx, id, &types.UpdateUserRequest{Name: ptrStr("crud-user-2")})
		rec.step(t, "Users.Update", err)
		rec.step(t, "Users.Delete", sdk.Governance.Users.Delete(ctx, id))
	})

	// ---- Governance: VirtualKey (-> team, provider) -----------------------
	var vkID string
	t.Run("virtual_key", func(t *testing.T) {
		req := &types.CreateVirtualKeyRequest{
			Name: "crud-vk",
			ProviderConfigs: []types.CreateVKProviderConfig{{
				Provider:      "openai",
				AllowedModels: schemas.WhiteList{"*"},
				KeyIDs:        schemas.WhiteList{"*"},
			}},
		}
		if teamID != "" {
			req.TeamID = &teamID
		}
		r, err := sdk.Governance.VirtualKeys.Create(ctx, req)
		if !rec.step(t, "VirtualKeys.Create", err) {
			return
		}
		vkID = r.VirtualKey.ID
		// FK parent (referenced by pricing_override) — deleted inline at body end.
		_, err = sdk.Governance.VirtualKeys.Get(ctx, vkID)
		rec.step(t, "VirtualKeys.Get", err)
		_, err = sdk.Governance.VirtualKeys.List(ctx, nil)
		rec.step(t, "VirtualKeys.List", err)
		_, err = sdk.Governance.VirtualKeys.Update(ctx, vkID, &types.UpdateVirtualKeyRequest{Description: ptrStr("updated")})
		rec.step(t, "VirtualKeys.Update", err)
		rec.step(t, "VirtualKeys.Rotate", sdk.Governance.VirtualKeys.Rotate(ctx, vkID))
		rec.step(t, "VirtualKeys.BulkRotate", sdk.Governance.VirtualKeys.BulkRotate(ctx, []string{vkID}))
		// GetQuota is a VK-scoped call (needs the x-bf-vk header), not an admin
		// CRUD op — out of scope here.
	})

	// ---- Governance: RoutingRule ------------------------------------------
	t.Run("routing_rule", func(t *testing.T) {
		r, err := sdk.Governance.RoutingRules.Create(ctx, &types.CreateRoutingRuleRequest{
			Name:          "crud-rule",
			CelExpression: `request_type == "chat_completion"`,
			Targets:       []types.RoutingTarget{{Provider: ptrStr("openai"), Model: ptrStr("gpt-4o-mock"), Weight: 1}},
		})
		if !rec.step(t, "RoutingRules.Create", err) {
			return
		}
		id := r.Rule.ID
		t.Cleanup(func() { _ = sdk.Governance.RoutingRules.Delete(context.Background(), id) })
		_, err = sdk.Governance.RoutingRules.Get(ctx, id)
		rec.step(t, "RoutingRules.Get", err)
		_, err = sdk.Governance.RoutingRules.List(ctx, nil)
		rec.step(t, "RoutingRules.List", err)
		_, err = sdk.Governance.RoutingRules.Update(ctx, id, &types.UpdateRoutingRuleRequest{Description: ptrStr("updated")})
		rec.step(t, "RoutingRules.Update", err)
		rec.step(t, "RoutingRules.Delete", sdk.Governance.RoutingRules.Delete(ctx, id))
	})

	// ---- Governance: ModelConfig ------------------------------------------
	t.Run("model_config", func(t *testing.T) {
		r, err := sdk.Governance.ModelConfigs.Create(ctx, &types.CreateModelConfigRequest{
			ModelName: "gpt-4o-mock", Provider: ptrStr("openai"),
		})
		if !rec.step(t, "ModelConfigs.Create", err) {
			return
		}
		id := r.ModelConfig.ID
		t.Cleanup(func() { _ = sdk.Governance.ModelConfigs.Delete(context.Background(), id) })
		_, err = sdk.Governance.ModelConfigs.Get(ctx, id)
		rec.step(t, "ModelConfigs.Get", err)
		_, err = sdk.Governance.ModelConfigs.List(ctx, nil)
		rec.step(t, "ModelConfigs.List", err)
		_, err = sdk.Governance.ModelConfigs.Update(ctx, id, &types.UpdateModelConfigRequest{ModelName: ptrStr("gpt-4o-mock-2")})
		rec.step(t, "ModelConfigs.Update", err)
		rec.step(t, "ModelConfigs.Delete", sdk.Governance.ModelConfigs.Delete(ctx, id))
	})

	// ---- Governance: PricingOverride (-> VK) ------------------------------
	t.Run("pricing_override", func(t *testing.T) {
		if vkID == "" {
			t.Skip("no virtual key available")
		}
		r, err := sdk.Governance.PricingOverrides.Create(ctx, &types.CreatePricingOverrideRequest{
			Name:         "crud-pricing",
			ScopeKind:    types.ScopeKindVirtualKey,
			VirtualKeyID: &vkID,
			MatchType:    types.MatchTypeExact,
			Pattern:      "gpt-4o-mock",
			RequestTypes: []schemas.RequestType{schemas.ChatCompletionRequest},
			Patch:        types.PricingOptions{InputCostPerToken: ptrF64(0.001)},
		})
		if !rec.step(t, "PricingOverrides.Create", err) {
			return
		}
		id := r.PricingOverride.ID
		t.Cleanup(func() { _ = sdk.Governance.PricingOverrides.Delete(context.Background(), id) })
		_, err = sdk.Governance.PricingOverrides.List(ctx, nil)
		rec.step(t, "PricingOverrides.List", err)
		_, err = sdk.Governance.PricingOverrides.Update(ctx, id, &types.UpdatePricingOverrideRequest{
			Pattern:      ptrStr("gpt-4o-mock-2"),
			RequestTypes: []schemas.RequestType{schemas.ChatCompletionRequest},
		})
		rec.step(t, "PricingOverrides.Update", err)
		rec.step(t, "PricingOverrides.Delete", sdk.Governance.PricingOverrides.Delete(ctx, id))
	})

	// ---- Governance: ProviderGovernance (no Create/Get; per-provider) -----
	t.Run("provider_governance", func(t *testing.T) {
		_, err := sdk.Governance.Providers.List(ctx)
		rec.step(t, "ProviderGovernance.List", err)
		_, err = sdk.Governance.Providers.Update(ctx, "openai", &types.UpdateProviderGovernanceRequest{
			RateLimit: &types.UpdateRateLimitRequest{RequestMaxLimit: ptrI64(1000), RequestResetDuration: ptrStr("1h")},
		})
		if rec.step(t, "ProviderGovernance.Update", err) {
			rec.step(t, "ProviderGovernance.Delete", sdk.Governance.Providers.Delete(ctx, "openai"))
		}
	})

	// ---- Governance budgets / rate-limits (list only) ---------------------
	t.Run("governance_lists", func(t *testing.T) {
		_, err := sdk.Governance.ListBudgets(ctx)
		rec.step(t, "Governance.ListBudgets", err)
		_, err = sdk.Governance.ListRateLimits(ctx)
		rec.step(t, "Governance.ListRateLimits", err)
	})

	// ---- Plugins (skipped for now) ----------------------------------------
	// Plugin Create loads the plugin by name, so it only accepts known built-ins
	// with valid config — not a clean generic CRUD lifecycle. Deferred.

	// ---- MCP client -------------------------------------------------------
	t.Run("mcp", func(t *testing.T) {
		// Points at the in-cluster mock MCP server (streamable HTTP) so bifrost
		// connects + lists tools on create. Supply client_id so we can address
		// the client for Update/Delete without relying on the create response
		// (whose Config pointer may be nil). (Server rejects hyphens in names.)
		const mcpID = "crudmcp"
		r, err := sdk.MCP.Create(ctx, &types.CreateMCPClientRequest{
			ID:               mcpID,
			Name:             mcpID,
			ConnectionType:   schemas.MCPConnectionTypeHTTP,
			ConnectionString: &schemas.EnvVar{Val: fixtures.MockMCPInClusterURL},
			AuthType:         schemas.MCPAuthTypeNone,
		})
		if !rec.step(t, "MCP.Create", err) {
			return
		}
		id := mcpID
		if r != nil && r.Config != nil && r.Config.ID != "" {
			id = r.Config.ID
		}
		_, err = sdk.MCP.Update(ctx, id, &types.UpdateMCPClientRequest{Name: ptrStr("crudmcp2")})
		rec.step(t, "MCP.Update", err)
		rec.step(t, "MCP.Delete", sdk.MCP.Delete(ctx, id))
	})

	// ---- Prompts (+ folder, version, session) -----------------------------
	t.Run("prompts", func(t *testing.T) {
		// Folder
		var folderID string
		if fr, err := sdk.Prompts.Folders.Create(ctx, &types.CreateFolderRequest{Name: "crud-folder"}); rec.step(t, "Folders.Create", err) && fr != nil {
			folderID = fr.ID
			t.Cleanup(func() { _ = sdk.Prompts.Folders.Delete(context.Background(), folderID) })
			_, err = sdk.Prompts.Folders.Get(ctx, folderID)
			rec.step(t, "Folders.Get", err)
			_, err = sdk.Prompts.Folders.List(ctx)
			rec.step(t, "Folders.List", err)
			_, err = sdk.Prompts.Folders.Update(ctx, folderID, &types.UpdateFolderRequest{Name: "crud-folder-2"})
			rec.step(t, "Folders.Update", err)
		}

		// Prompt
		pr, err := sdk.Prompts.Create(ctx, &types.CreatePromptRequest{Name: "crud-prompt"})
		if !rec.step(t, "Prompts.Create", err) || pr == nil {
			return
		}
		promptID := pr.ID
		t.Cleanup(func() { _ = sdk.Prompts.Delete(context.Background(), promptID) })
		_, err = sdk.Prompts.Get(ctx, promptID)
		rec.step(t, "Prompts.Get", err)
		_, err = sdk.Prompts.List(ctx)
		rec.step(t, "Prompts.List", err)
		_, err = sdk.Prompts.Update(ctx, promptID, &types.UpdatePromptRequest{Name: "crud-prompt-2"})
		rec.step(t, "Prompts.Update", err)

		// Version (under the prompt)
		if vr, err := sdk.Prompts.Versions.Create(ctx, promptID, &types.CreateVersionRequest{
			CommitMessage: "v1",
			Messages:      []types.PromptMessage{{Role: "user", Content: "hello"}},
			ModelParams:   types.ModelParams{Temperature: ptrF64(0.7)},
			Provider:      "openai", Model: "gpt-4o-mock",
		}); rec.step(t, "Versions.Create", err) && vr != nil {
			_, err = sdk.Prompts.Versions.Get(ctx, vr.ID)
			rec.step(t, "Versions.Get", err)
			_, err = sdk.Prompts.Versions.List(ctx, promptID)
			rec.step(t, "Versions.List", err)
			rec.step(t, "Versions.Delete", sdk.Prompts.Versions.Delete(ctx, vr.ID))
		}

		// Session (under the prompt)
		if sr, err := sdk.Prompts.Sessions.Create(ctx, promptID, &types.CreateSessionRequest{
			Name: "crud-session", Provider: "openai", Model: "gpt-4o-mock",
			ModelParams: types.ModelParams{Temperature: ptrF64(0.7)},
		}); rec.step(t, "Sessions.Create", err) && sr != nil {
			sid := sr.ID
			_, err = sdk.Prompts.Sessions.Get(ctx, sid)
			rec.step(t, "Sessions.Get", err)
			_, err = sdk.Prompts.Sessions.List(ctx, promptID)
			rec.step(t, "Sessions.List", err)
			_, err = sdk.Prompts.Sessions.Rename(ctx, sid, "crud-session-2")
			rec.step(t, "Sessions.Rename", err)
			rec.step(t, "Sessions.Delete", sdk.Prompts.Sessions.Delete(ctx, sid))
		}

		rec.step(t, "Prompts.Delete", sdk.Prompts.Delete(ctx, promptID))
	})

	// ---- FK-chain deletes: inline (port-forward still open), reverse order --
	// vk (referenced by pricing_override) -> team (by user/vk) -> customer (by team).
	if vkID != "" {
		rec.step(t, "VirtualKeys.Delete", sdk.Governance.VirtualKeys.Delete(ctx, vkID))
	}
	if teamID != "" {
		rec.step(t, "Teams.Delete", sdk.Governance.Teams.Delete(ctx, teamID))
	}
	if custID != "" {
		rec.step(t, "Customers.Delete", sdk.Governance.Customers.Delete(ctx, custID))
	}
}

// ---- classification harness ------------------------------------------------

type crudOutcome int

const (
	outPass crudOutcome = iota
	outUnimpl
	outFail
)

func (o crudOutcome) String() string {
	switch o {
	case outPass:
		return "PASS"
	case outUnimpl:
		return "UNIMPLEMENTED"
	default:
		return "FAIL"
	}
}

type stepResult struct {
	name   string
	out    crudOutcome
	status int
	err    error
}

type recorder struct {
	mu      sync.Mutex
	results []stepResult
}

// step classifies the result of an SDK call, records it, logs it, and marks the
// (sub)test failed only on a real FAIL. Returns true iff the call PASSed.
func (r *recorder) step(t *testing.T, name string, err error) bool {
	t.Helper()
	out, status := classify(err)
	r.mu.Lock()
	r.results = append(r.results, stepResult{name, out, status, err})
	r.mu.Unlock()
	switch out {
	case outPass:
		t.Logf("PASS          %s", name)
	case outUnimpl:
		t.Logf("UNIMPL (%3d)  %s", status, name)
	default:
		t.Errorf("FAIL   (%3d)  %s: %v", status, name, err)
	}
	return out == outPass
}

// classify maps an SDK error to an outcome. nil -> PASS; a 404/405/501 APIError
// -> UNIMPLEMENTED (endpoint not served); anything else -> FAIL.
func classify(err error) (crudOutcome, int) {
	if err == nil {
		return outPass, 200
	}
	var ae *bifrostai.APIError
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case 404, 405, 501:
			return outUnimpl, ae.StatusCode
		default:
			return outFail, ae.StatusCode
		}
	}
	return outFail, 0
}

func (r *recorder) report(t *testing.T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var p, u, f int
	var lines []string
	for _, s := range r.results {
		switch s.out {
		case outPass:
			p++
		case outUnimpl:
			u++
			lines = append(lines, fmt.Sprintf("  UNIMPLEMENTED (%d)  %s", s.status, s.name))
		default:
			f++
			lines = append(lines, fmt.Sprintf("  FAIL          (%d)  %s: %v", s.status, s.name, s.err))
		}
	}
	t.Logf("==== CRUD API sanity: %d PASS, %d UNIMPLEMENTED, %d FAIL (of %d calls) ====", p, u, f, len(r.results))
	for _, l := range lines {
		t.Log(l)
	}
}

var _ = time.Second
