// Tests covering the GORM tenant-scope callback registered by
// RegisterTenantScopes and the data paths that interact with it.
//
// Regression-test conventions:
//   - Every test wires its own RegisterTenantScopes-equipped *gorm.DB
//     via setupRDBTestStoreWithTenantScope, because the default
//     setupRDBTestStore (in rdb_test.go) does not register the callback
//     and therefore cannot reproduce tenant-isolation bugs.
//   - Tenant identity is pushed onto the context with the typed
//     BifrostContextKeyTenantID — same form the HTTP-layer resolver
//     middleware uses.

package configstore

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupRDBTestStoreWithTenantScope is the tenant-aware sibling of
// setupRDBTestStore. It additionally registers the four scope
// callbacks so SELECT/UPDATE/DELETE on tenant-scoped tables get
// `WHERE tenant_id = ?` from ctx and INSERTs get tenant_id populated.
func setupRDBTestStoreWithTenantScope(t *testing.T) *RDBConfigStore {
	store := setupRDBTestStore(t)
	require.NoError(t, RegisterTenantScopes(store.DB()), "register tenant-scope callbacks")
	return store
}

// withTenant returns a context carrying the supplied tenant id under
// multitenant.BifrostContextKeyTenantID — the form the configstore
// tenant-scope callback reads.
func withTenant(parent context.Context, tid string) context.Context {
	return context.WithValue(parent, multitenant.BifrostContextKeyTenantID, tid)
}

// TestUpdateProviderKey_PreservesTenantID is the regression for a
// silent data-corruption bug:
//
//   - UpdateProviderKey reconstructs a TableKey from a stub
//     TableProvider (only ID + Name) and never copies the existing
//     row's TenantID onto the struct.
//   - gorm.Save then issues UPDATE SET tenant_id = '' WHERE id = ? AND
//     tenant_id = '<ctx tenant>' (the WHERE comes from the scope
//     callback). The row's tenant_id is overwritten to empty string.
//   - Subsequent reads filtered by tenant_id = '<ctx tenant>' return
//     ErrNotFound; the row is orphaned.
//
// Symptom in production: a PUT /api/providers/{p}/keys/{kid} returns
// 404 on the NEXT update attempt, even though the key is visibly still
// in the DB.
func TestUpdateProviderKey_PreservesTenantID(t *testing.T) {
	store := setupRDBTestStoreWithTenantScope(t)
	ctx := withTenant(context.Background(), tables.DefaultTenantID)

	// Provider + key in the default tenant scope.
	require.NoError(t, store.AddProvider(ctx, "openai", ProviderConfig{}))
	originalKey := schemas.Key{
		ID:     "key-uuid-1",
		Name:   "openai-primary",
		Value:  *schemas.NewEnvVar("sk-original"),
		Weight: 1.0,
		Models: []string{"gpt-4"},
	}
	require.NoError(t, store.CreateProviderKey(ctx, "openai", originalKey))

	// Pre-flight: verify the row was created with the expected tenant_id.
	var rowBefore tables.TableKey
	require.NoError(t, store.DB().WithContext(context.Background()).
		Where("key_id = ?", originalKey.ID).First(&rowBefore).Error)
	require.Equal(t, tables.DefaultTenantID, rowBefore.TenantID,
		"freshly-created key row must carry the ctx tenant on tenant_id")

	// Update the model list — the UI-equivalent of the failing
	// PUT /api/providers/{p}/keys/{kid} from the field report.
	updated := originalKey
	updated.Models = []string{"gpt-4", "gpt-4-turbo"}
	require.NoError(t, store.UpdateProviderKey(ctx, "openai", originalKey.ID, updated),
		"UpdateProviderKey under tenant ctx must succeed")

	// Critical assertion: the row's tenant_id must NOT have been
	// overwritten to the empty string by the UPDATE. If this fails,
	// every subsequent tenant-scoped read of the key will 404.
	var rowAfter tables.TableKey
	require.NoError(t, store.DB().WithContext(context.Background()).
		Where("key_id = ?", originalKey.ID).First(&rowAfter).Error)
	assert.Equal(t, tables.DefaultTenantID, rowAfter.TenantID,
		"UpdateProviderKey must not zero out the row's tenant_id; "+
			"the GORM tenant-scope callback would then 404 every "+
			"subsequent read under the same tenant header.")

	// Belt and braces: re-fetch via the public API (which goes through
	// the scope callback). Before the fix this 404s with ErrNotFound.
	roundTrip, err := store.GetProviderKey(ctx, "openai", originalKey.ID)
	require.NoError(t, err, "GetProviderKey after UpdateProviderKey must not 404")
	require.NotNil(t, roundTrip)
	assert.ElementsMatch(t, []string{"gpt-4", "gpt-4-turbo"}, roundTrip.Models,
		"the model list update should round-trip")
}
