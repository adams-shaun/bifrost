package governance

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
)

// Plugin isolation POC — see docs/multi-tenant-f5/06-plugin-isolation.md
//
// Goal: prove that constructing two GovernancePlugin instances (one per
// tenant) gives true state isolation — no map keys, counters, or background
// goroutines bleed between them. This is the kernel of the per-tenant plugin
// instance model the design doc proposes.
//
// What this POC test DOES validate:
//   - LocalGovernanceStore's in-memory sync.Maps are per-instance (changes
//     to instance A's budget do not appear on instance B even when both
//     started from the same seed config — this models two tenants whose
//     configstore rows happen to define the SAME budget id)
//   - BumpBudgetUsage on one instance leaves the sibling instance's snapshot
//     untouched (the canary for cross-tenant accounting leaks; the I8
//     isolation stub in examples/isolation/main.go cares about this)
//   - Per-instance LastReset / CurrentUsage tracking via UpsertBudgetConfig
//     preserves snapshots correctly across both instances independently
//
// What this POC DELIBERATELY DOES NOT exercise (called out in the doc):
//   - The loadFromDatabase path with the GORM tenant-scope callback. The
//     mechanism already exists (patch2) and is unit-tested in the
//     configstore package. This test uses the in-memory loadFromConfigMemory
//     path because it's the cheapest way to seed both instances with
//     identical-by-design state and prove they stay separate.
//   - Wiring per-tenant instances into the TenantLoader at
//     transports/bifrost-http/server/multitenant.go::newTenantLoader. That's
//     the production glue the design doc proposes; the test below proves the
//     model is sound BEFORE we touch the loader.
//   - The tracker's background goroutines. NewUsageTracker spawns workers
//     with context.Background() — a real isolation concern called out in
//     the doc — but the test doesn't stress the worker path because the
//     BumpBudgetUsage entry point is the in-memory choke point that
//     workers eventually call into. If sync.Map isolation holds at the
//     entry point, worker writes can't cross either.
func TestPluginIsolationPOC_TwoTenantsSameBudgetID(t *testing.T) {
	logger := NewMockLogger()
	ctx := context.Background()

	// Both tenants happen to define the SAME budget id "monthly-cap"
	// (realistic — admins copying templates across tenants). The point of the
	// POC is: even with colliding ids, per-instance maps must not merge.
	const sharedBudgetID = "monthly-cap"
	seed := func(initialUsage float64) *configstore.GovernanceConfig {
		return &configstore.GovernanceConfig{
			Budgets: []configstoreTables.TableBudget{
				*buildBudgetWithUsage(sharedBudgetID, 100.0, initialUsage, "1h"),
			},
		}
	}

	// Construct two stores from configs whose Budgets list has the SAME id but
	// different starting usage. If the model bleeds, both stores will end up
	// with identical CurrentUsage after either side is bumped.
	storeA, err := NewLocalGovernanceStore(ctx, logger, nil, seed(0), nil)
	require.NoError(t, err)
	storeB, err := NewLocalGovernanceStore(ctx, logger, nil, seed(0), nil)
	require.NoError(t, err)

	// Sanity: both see the seed.
	bA := storeA.LoadBudget(ctx, sharedBudgetID)
	bB := storeB.LoadBudget(ctx, sharedBudgetID)
	require.NotNil(t, bA, "storeA should see seeded budget")
	require.NotNil(t, bB, "storeB should see seeded budget")
	require.Equal(t, 0.0, bA.CurrentUsage)
	require.Equal(t, 0.0, bB.CurrentUsage)

	// Bump on storeA only.
	require.NoError(t, storeA.BumpBudgetUsage(ctx, sharedBudgetID, 25.0))

	bA = storeA.LoadBudget(ctx, sharedBudgetID)
	bB = storeB.LoadBudget(ctx, sharedBudgetID)
	require.Equal(t, 25.0, bA.CurrentUsage, "storeA's bump should land on storeA")
	require.Equal(t, 0.0, bB.CurrentUsage, "storeA's bump MUST NOT leak to storeB (cross-tenant accounting leak)")

	// Bump on storeB independently.
	require.NoError(t, storeB.BumpBudgetUsage(ctx, sharedBudgetID, 7.0))

	bA = storeA.LoadBudget(ctx, sharedBudgetID)
	bB = storeB.LoadBudget(ctx, sharedBudgetID)
	require.Equal(t, 25.0, bA.CurrentUsage, "storeA's value should be unaffected by storeB's bump")
	require.Equal(t, 7.0, bB.CurrentUsage, "storeB's bump should land on storeB only")

	// UpsertBudgetConfig preserves per-instance CurrentUsage across config
	// replacement (admin edit of MaxLimit). Replace the budget on storeA only
	// and confirm storeA keeps its 25.0 usage AND storeB stays at 7.0.
	newCfg := buildBudgetWithUsage(sharedBudgetID, 200.0, 999.0, "1h") // 999 is bait — should be discarded by UpsertBudgetConfig
	storeA.UpsertBudgetConfig(ctx, sharedBudgetID, newCfg)

	bA = storeA.LoadBudget(ctx, sharedBudgetID)
	bB = storeB.LoadBudget(ctx, sharedBudgetID)
	require.Equal(t, 200.0, bA.MaxLimit, "storeA's MaxLimit edit should apply to A")
	require.Equal(t, 25.0, bA.CurrentUsage, "UpsertBudgetConfig must preserve A's in-memory CurrentUsage despite new config's bait value")
	require.Equal(t, 100.0, bB.MaxLimit, "storeB's MaxLimit MUST remain at the original 100.0 — admin edit on A must not bleed")
	require.Equal(t, 7.0, bB.CurrentUsage, "storeB's CurrentUsage must remain 7.0 throughout")
}

// TestPluginIsolationPOC_FullPluginInstances goes one level up from the store
// test: it constructs two FULL GovernancePlugin instances via Init (which
// internally builds store + resolver + tracker + engine + per-instance ctx)
// and confirms the per-instance state still isolates after the whole stack
// is wired together. This is the "would the design doc's TenantLoader-built
// per-tenant plugin model actually work?" check.
//
// Constraints surfaced while writing this:
//
//   1. NewUsageTracker (tracker.go:63) creates its background context with
//      context.Background() — NOT derived from the plugin's tenant-scoped
//      ctx. Two instances thus get two independent worker pools (good), but
//      the workers themselves run with an UNSCOPED context. If/when the
//      tracker workers do reads through configStore (e.g. the periodic
//      reset worker), those reads won't have a tenant on ctx, so the GORM
//      scope callback won't filter. This is fine in the per-tenant model
//      ONLY IF the worker's DB scope is bounded by the budget id it's
//      operating on (which is unique-by-tenant). Spelled out in the design
//      doc as Constraint #3.
//
//   2. GovernancePlugin.Cleanup() is per-instance — calling it on instance
//      A doesn't touch B. So a TenantLoader-built per-tenant plugin can be
//      cleanly torn down when its owning tenant runtime is evicted by
//      multitenant.Manager. No ShareLLMPlugins shim needed for this lane.
//      That's a real win — the shim only existed to NO-OP Cleanup so a
//      per-tenant Shutdown couldn't kill the singleton's worker pool.
//
//   3. The plugin's Init call signature is heavy (8 args including a config
//      store, a model catalog, an MCP catalog, an in-memory store). For a
//      TenantLoader to build per-tenant instances, it needs all of those
//      handles. Most are already process-globals the loader has (logger,
//      configStore, modelCatalog) — but the inMemoryStore is currently
//      shared via ShareLLMPlugins. Whether per-tenant instances need their
//      own inMemoryStore vs sharing the root's is an open question
//      surfaced in the doc.
func TestPluginIsolationPOC_FullPluginInstances(t *testing.T) {
	logger := NewMockLogger()
	ctx := context.Background()
	const sharedBudgetID = "team-cap"

	seed := func() *configstore.GovernanceConfig {
		return &configstore.GovernanceConfig{
			Budgets: []configstoreTables.TableBudget{
				*buildBudget(sharedBudgetID, 100.0, "1h"),
			},
		}
	}

	pluginA, err := Init(ctx, nil, logger, nil, seed(), nil, nil, nil)
	require.NoError(t, err)
	defer func() { _ = pluginA.Cleanup() }()

	pluginB, err := Init(ctx, nil, logger, nil, seed(), nil, nil, nil)
	require.NoError(t, err)
	defer func() { _ = pluginB.Cleanup() }()

	// BumpBudgetUsage is a method on *LocalGovernanceStore, not on the
	// GovernanceStore interface (interface exposes the higher-level
	// Update*UsageInMemory wrappers). The plugin's store field is interface-
	// typed, so we cast for direct access in the test.
	storeA := pluginA.store.(*LocalGovernanceStore)
	storeB := pluginB.store.(*LocalGovernanceStore)

	// Move A only.
	require.NoError(t, storeA.BumpBudgetUsage(ctx, sharedBudgetID, 30.0))

	bA := storeA.LoadBudget(ctx, sharedBudgetID)
	bB := storeB.LoadBudget(ctx, sharedBudgetID)
	require.NotNil(t, bA)
	require.NotNil(t, bB)
	require.Equal(t, 30.0, bA.CurrentUsage, "pluginA's usage")
	require.Equal(t, 0.0, bB.CurrentUsage, "pluginB's usage MUST be untouched by pluginA's bump")

	// Confirm Cleanup on A doesn't take down B's background workers.
	require.NoError(t, pluginA.Cleanup())

	// B should still be operational.
	require.NoError(t, storeB.BumpBudgetUsage(ctx, sharedBudgetID, 12.0))
	bB = storeB.LoadBudget(ctx, sharedBudgetID)
	require.Equal(t, 12.0, bB.CurrentUsage, "pluginB should keep functioning after pluginA.Cleanup")
}

// TestPluginIsolationPOC_DeleteScopedToInstance confirms that
// DeleteBudget on instance A leaves instance B's snapshot intact. Models a
// tenant deleting a budget that happens to share its id with another
// tenant's still-active budget.
func TestPluginIsolationPOC_DeleteScopedToInstance(t *testing.T) {
	logger := NewMockLogger()
	ctx := context.Background()
	const sharedBudgetID = "campaign-q4"
	seed := func() *configstore.GovernanceConfig {
		return &configstore.GovernanceConfig{
			Budgets: []configstoreTables.TableBudget{
				*buildBudget(sharedBudgetID, 50.0, "1h"),
			},
		}
	}

	storeA, err := NewLocalGovernanceStore(ctx, logger, nil, seed(), nil)
	require.NoError(t, err)
	storeB, err := NewLocalGovernanceStore(ctx, logger, nil, seed(), nil)
	require.NoError(t, err)

	require.NotNil(t, storeA.LoadBudget(ctx, sharedBudgetID))
	require.NotNil(t, storeB.LoadBudget(ctx, sharedBudgetID))

	storeA.DeleteBudget(ctx, sharedBudgetID)

	require.Nil(t, storeA.LoadBudget(ctx, sharedBudgetID), "delete on A should clear A's snapshot")
	require.NotNil(t, storeB.LoadBudget(ctx, sharedBudgetID), "delete on A MUST NOT clear B's snapshot")
}
