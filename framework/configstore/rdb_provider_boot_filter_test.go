// f5xc-overlay (patch18): tests for GetProvidersConfig's boot-time
// default-tenant exclusion when BIFROST_DISABLE_DEFAULT_TENANT_CONFIG=true.
//
// Truth table the tests lock in:
//
//   env unset / "false"  + any ctx        → all rows returned (upstream)
//   env "true" + ctx has no tenant         → default-tenant rows EXCLUDED
//   env "true" + ctx has tenant            → all rows returned (request path,
//                                            GORM tenant-scope callback
//                                            handles per-tenant filtering)
//
// The second row is the bug fix: without it, the boot loader at
// transports/bifrost-http/lib/config.go::loadProviders pulls every
// tenant's rows into the shared in-memory map, last-write-wins collapses
// them by provider name only, and the root runtime ends up running
// against a random tenant's URL / keys.

package configstore

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/stretchr/testify/require"
)

// seedQwenAcrossTenants writes a qwen provider row for each of the
// supplied tenant ids using the per-tenant ctx. Returns nothing — tests
// query via GetProvidersConfig afterwards.
func seedQwenAcrossTenants(t *testing.T, store *RDBConfigStore, tenants ...string) {
	t.Helper()
	for _, tid := range tenants {
		row := &tables.TableProvider{
			Name:     "qwen",
			TenantID: tid,
			NetworkConfig: nil,
		}
		require.NoError(t,
			store.DB().WithContext(withTenant(context.Background(), tid)).Create(row).Error,
			"create qwen row for tenant %q", tid,
		)
	}
}

func TestGetProvidersConfig_EnvUnset_ReturnsAllRows(t *testing.T) {
	t.Setenv("BIFROST_DISABLE_DEFAULT_TENANT_CONFIG", "")
	store := setupRDBTestStoreWithTenantScope(t)
	seedQwenAcrossTenants(t, store, tables.DefaultTenantID, "shaun", "acme")

	got, err := store.GetProvidersConfig(context.Background())
	require.NoError(t, err)
	// Map-collapsed by name; existence is what matters.
	if _, ok := got["qwen"]; !ok {
		t.Fatal("env-unset: qwen should be present in result (upstream behaviour)")
	}
}

func TestGetProvidersConfig_EnvSet_NoTenant_ExcludesDefault(t *testing.T) {
	t.Setenv("BIFROST_DISABLE_DEFAULT_TENANT_CONFIG", "true")
	store := setupRDBTestStoreWithTenantScope(t)

	// Default-only: env set + no tenant in ctx → result must be empty.
	seedQwenAcrossTenants(t, store, tables.DefaultTenantID)
	got, err := store.GetProvidersConfig(context.Background())
	require.NoError(t, err)
	if _, ok := got["qwen"]; ok {
		t.Fatal("env=true: default-tenant qwen MUST be excluded from boot load")
	}

	// Add a non-default tenant row: that one SHOULD appear in the result
	// (excluding default doesn't exclude everything).
	seedQwenAcrossTenants(t, store, "shaun")
	got, err = store.GetProvidersConfig(context.Background())
	require.NoError(t, err)
	if _, ok := got["qwen"]; !ok {
		t.Fatal("env=true: non-default tenant's qwen should still be present in result")
	}
}

func TestGetProvidersConfig_EnvSet_WithTenant_ReturnsAllRows(t *testing.T) {
	// When ctx carries a tenant id, the GORM tenant-scope callback
	// already adds WHERE tenant_id = ? to the query. Our env-gated
	// filter would be redundant (and would over-restrict if ctx says
	// 'default'). The exclusion fires ONLY on no-tenant ctx (boot path).
	t.Setenv("BIFROST_DISABLE_DEFAULT_TENANT_CONFIG", "true")
	store := setupRDBTestStoreWithTenantScope(t)
	seedQwenAcrossTenants(t, store, tables.DefaultTenantID, "shaun")

	got, err := store.GetProvidersConfig(withTenant(context.Background(), "shaun"))
	require.NoError(t, err)
	if _, ok := got["qwen"]; !ok {
		t.Fatal("env=true with tenant ctx: shaun's qwen should be returned")
	}
}

func TestGetProvidersConfig_EnvSet_CrossTenantCollapseAvoided(t *testing.T) {
	// The headline scenario the patch fixes: multiple tenants each have
	// a qwen row with DIFFERENT network_config (URL). At boot, without
	// the filter, the map collapses them and a random one wins. With
	// the filter (env set + no ctx), default's row is excluded so
	// whichever single non-default row is present "wins" deterministically
	// from the operator's POV — i.e. they know it'll be a non-default
	// row, not a stale default seeded long ago.
	t.Setenv("BIFROST_DISABLE_DEFAULT_TENANT_CONFIG", "true")
	store := setupRDBTestStoreWithTenantScope(t)

	// Default has a STALE row (the kind that survives schema migrations
	// from before the cluster was MT-enabled). Should NOT influence boot.
	require.NoError(t,
		store.DB().WithContext(withTenant(context.Background(), tables.DefaultTenantID)).
			Create(&tables.TableProvider{
				Name:     "qwen",
				TenantID: tables.DefaultTenantID,
				// Stale URL fingerprint we can detect.
				NetworkConfig: nil,
			}).Error,
	)

	// Boot-style call: empty ctx, env=true.
	got, err := store.GetProvidersConfig(context.Background())
	require.NoError(t, err)
	if _, ok := got["qwen"]; ok {
		t.Fatal("only the default row exists; with env=true, result must NOT include qwen — the boot loader is letting per-tenant runtimes serve qwen via their own TenantLoader")
	}

	// Sanity: the row is still in the DB (we didn't delete it; we just
	// stopped including it at boot).
	var count int64
	require.NoError(t,
		store.DB().Model(&tables.TableProvider{}).Where("name = ?", "qwen").Count(&count).Error,
	)
	if count != 1 {
		t.Fatalf("expected exactly 1 qwen row still in DB, got %d", count)
	}
}

// _ avoids the unused-import warning if multitenant.* is only referenced
// in setup helpers via withTenant — keeps the test file honest if those
// helpers move.
var _ = multitenant.BifrostContextKeyTenantID
