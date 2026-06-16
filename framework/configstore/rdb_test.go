package configstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupRDBTestStore creates an in-memory SQLite database and returns an RDBConfigStore for testing
func setupRDBTestStore(t *testing.T) *RDBConfigStore {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err, "Failed to create test database")

	// Run migrations for all tables
	err = db.AutoMigrate(
		&tables.TableProvider{},
		&tables.TableKey{},
		&tables.TableBudget{},
		&tables.TableRateLimit{},
		&tables.TableVirtualKey{},
		&tables.TableVirtualKeyProviderConfig{},
		&tables.TableVirtualKeyProviderConfigKey{},
		&tables.TableCustomer{},
		&tables.TableTeam{},
		&tables.TableTenant{},
		&tables.TableClientConfig{},
		&tables.TablePlugin{},
		&tables.TableMCPClient{},
		&tables.TableVirtualKeyMCPConfig{},
		&tables.TableFolder{},
		&tables.TablePrompt{},
		&tables.TablePromptVersion{},
		&tables.TablePromptVersionMessage{},
		&tables.TablePromptSession{},
		&tables.TablePromptSessionMessage{},
		&tables.TableOauthUserSession{},
		&tables.TableOauthUserToken{},
	)
	require.NoError(t, err, "Failed to migrate test database")

	// Seed the default tenant. The migration chain does this via the
	// initial multi-tenant migration; the minimal AutoMigrate above
	// skips that, so seed it explicitly so DeleteTenant's reserved-id
	// guard has something to point at.
	require.NoError(t, db.Create(&tables.TableTenant{
		ID:     tables.DefaultTenantID,
		Name:   "Default Tenant",
		Status: tables.TenantStatusActive,
	}).Error)

	// Setup join table
	err = db.SetupJoinTable(&tables.TableVirtualKeyProviderConfig{}, "Keys", &tables.TableVirtualKeyProviderConfigKey{})
	require.NoError(t, err, "Failed to setup join table")

	s := &RDBConfigStore{logger: nil}
	s.db.Store(db)
	s.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, s.DB())
	}
	s.refreshPoolFn = func(ctx context.Context) error { return nil }
	return s
}

// =============================================================================
// Provider and Key Tests
// =============================================================================

func TestUpdateProvidersConfig_CreateNew(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{
					ID:     "key-uuid-1",
					Name:   "openai-primary",
					Value:  *schemas.NewEnvVar("sk-test-key"),
					Weight: 1.0,
				},
			},
		},
	}

	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Verify provider was created
	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Contains(t, result, schemas.ModelProvider("openai"))
	assert.Len(t, result["openai"].Keys, 1)
	assert.Equal(t, "openai-primary", result["openai"].Keys[0].Name)
}

func TestUpdateProvidersConfig_UpdateExistingByKeyID(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create initial provider with key
	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{
					ID:     "key-uuid-1",
					Name:   "openai-primary",
					Value:  *schemas.NewEnvVar("sk-test-key-v1"),
					Weight: 1.0,
				},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Update with same KeyID but different value
	providers["openai"] = ProviderConfig{
		Keys: []schemas.Key{
			{
				ID:     "key-uuid-1", // Same KeyID
				Name:   "openai-primary",
				Value:  *schemas.NewEnvVar("sk-test-key-v2"), // Updated value
				Weight: 2.0,
			},
		},
	}
	err = store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Verify key was updated, not duplicated
	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, result["openai"].Keys, 1)
	assert.Equal(t, "sk-test-key-v2", result["openai"].Keys[0].Value.Val)
}

func TestUpdateProvidersConfig_UpdateExistingByName_FallbackFix(t *testing.T) {
	// This test verifies the fix for the unique constraint violation issue
	// when a new UUID is generated for a key that already exists by name
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create initial provider with key
	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{
					ID:     "original-uuid",
					Name:   "openai-primary",
					Value:  *schemas.NewEnvVar("sk-test-key-v1"),
					Weight: 1.0,
				},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Simulate config reload with NEW UUID (as happens when loading from config file)
	providers["openai"] = ProviderConfig{
		Keys: []schemas.Key{
			{
				ID:     "new-uuid-from-config-reload", // Different UUID!
				Name:   "openai-primary",              // Same name
				Value:  *schemas.NewEnvVar("sk-test-key-v2"),
				Weight: 1.5,
			},
		},
	}
	err = store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err, "Should not fail with unique constraint violation")

	// Verify key was updated (not duplicated) and original KeyID preserved
	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, result["openai"].Keys, 1, "Should have exactly one key, not duplicated")
	assert.Equal(t, "sk-test-key-v2", result["openai"].Keys[0].Value.Val, "Value should be updated")
	assert.Equal(t, "original-uuid", result["openai"].Keys[0].ID, "Original KeyID should be preserved")
}

func TestUpdateProvidersConfig_MultipleKeys(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{ID: "key-1", Name: "openai-primary", Value: *schemas.NewEnvVar("sk-key-1"), Weight: 1.0},
				{ID: "key-2", Name: "openai-secondary", Value: *schemas.NewEnvVar("sk-key-2"), Weight: 0.5},
			},
		},
		"anthropic": {
			Keys: []schemas.Key{
				{ID: "key-3", Name: "anthropic-main", Value: *schemas.NewEnvVar("sk-key-3"), Weight: 1.0},
			},
		},
	}

	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 2)
	assert.Len(t, result["openai"].Keys, 2)
	assert.Len(t, result["anthropic"].Keys, 1)
}

func TestProviderKeyCRUD(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateProvidersConfig(ctx, map[schemas.ModelProvider]ProviderConfig{
		"openai": {},
	})
	require.NoError(t, err)

	keys, err := store.GetProviderKeys(ctx, "openai")
	require.NoError(t, err)
	assert.Empty(t, keys)

	key := schemas.Key{
		ID:     "key-uuid-1",
		Name:   "openai-primary",
		Value:  *schemas.NewEnvVar("sk-test-key-v1"),
		Weight: 1.0,
	}

	err = store.CreateProviderKey(ctx, "openai", key)
	require.NoError(t, err)

	keys, err = store.GetProviderKeys(ctx, "openai")
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, "openai-primary", keys[0].Name)

	storedKey, err := store.GetProviderKey(ctx, "openai", key.ID)
	require.NoError(t, err)
	require.NotNil(t, storedKey)
	assert.Equal(t, "sk-test-key-v1", storedKey.Value.Val)

	key.Value = *schemas.NewEnvVar("sk-test-key-v2")
	key.Weight = 2.0

	err = store.UpdateProviderKey(ctx, "openai", key.ID, key)
	require.NoError(t, err)

	storedKey, err = store.GetProviderKey(ctx, "openai", key.ID)
	require.NoError(t, err)
	require.NotNil(t, storedKey)
	assert.Equal(t, "sk-test-key-v2", storedKey.Value.Val)
	assert.Equal(t, 2.0, storedKey.Weight)

	err = store.DeleteProviderKey(ctx, "openai", key.ID)
	require.NoError(t, err)

	keys, err = store.GetProviderKeys(ctx, "openai")
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestProviderKeyCRUD_ProviderMustExist(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	key := schemas.Key{
		ID:     "key-uuid-1",
		Name:   "openai-primary",
		Value:  *schemas.NewEnvVar("sk-test-key-v1"),
		Weight: 1.0,
	}

	err := store.CreateProviderKey(ctx, "openai", key)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = store.GetProviderKeys(ctx, "openai")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = store.GetProviderKey(ctx, "openai", key.ID)
	require.ErrorIs(t, err, ErrNotFound)

	err = store.UpdateProviderKey(ctx, "openai", key.ID, key)
	require.ErrorIs(t, err, ErrNotFound)

	err = store.DeleteProviderKey(ctx, "openai", key.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

// =============================================================================
// Budget Tests
// =============================================================================

func TestCreateBudget(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	budget := &tables.TableBudget{
		ID:            "budget-test",
		MaxLimit:      100.0,
		ResetDuration: "1M",
	}

	err := store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Verify budget was created
	result, err := store.GetBudget(ctx, "budget-test")
	require.NoError(t, err)
	assert.Equal(t, "budget-test", result.ID)
	assert.Equal(t, 100.0, result.MaxLimit)
	assert.Equal(t, "1M", result.ResetDuration)
}

func TestCreateBudget_InvalidDuration(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	budget := &tables.TableBudget{
		ID:            "budget-invalid",
		MaxLimit:      100.0,
		ResetDuration: "invalid",
	}

	err := store.CreateBudget(ctx, budget)
	assert.Error(t, err, "Should fail with invalid duration")
}

func TestCreateBudget_NegativeLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	budget := &tables.TableBudget{
		ID:            "budget-negative",
		MaxLimit:      -50.0,
		ResetDuration: "1h",
	}

	err := store.CreateBudget(ctx, budget)
	assert.Error(t, err, "Should fail with negative max limit")
}

func TestUpdateBudget(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create budget
	budget := &tables.TableBudget{
		ID:            "budget-update",
		MaxLimit:      100.0,
		ResetDuration: "1h",
	}
	err := store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Update budget
	budget.MaxLimit = 200.0
	err = store.UpdateBudget(ctx, budget)
	require.NoError(t, err)

	// Verify update
	result, err := store.GetBudget(ctx, "budget-update")
	require.NoError(t, err)
	assert.Equal(t, 200.0, result.MaxLimit)
}

func TestGetBudgets(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create multiple budgets
	budgets := []*tables.TableBudget{
		{ID: "budget-1", MaxLimit: 100.0, ResetDuration: "1h"},
		{ID: "budget-2", MaxLimit: 200.0, ResetDuration: "1d"},
		{ID: "budget-3", MaxLimit: 300.0, ResetDuration: "1M"},
	}

	for _, b := range budgets {
		err := store.CreateBudget(ctx, b)
		require.NoError(t, err)
	}

	result, err := store.GetBudgets(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 3)
}

// =============================================================================
// Rate Limit Tests
// =============================================================================

func TestCreateRateLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)
	requestMax := int64(1000)
	tokenDuration := "1h"
	requestDuration := "1h"

	rateLimit := &tables.TableRateLimit{
		ID:                   "rate-limit-test",
		TokenMaxLimit:        &tokenMax,
		TokenResetDuration:   &tokenDuration,
		RequestMaxLimit:      &requestMax,
		RequestResetDuration: &requestDuration,
	}

	err := store.CreateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	result, err := store.GetRateLimit(ctx, "rate-limit-test")
	require.NoError(t, err)
	assert.Equal(t, "rate-limit-test", result.ID)
	assert.Equal(t, int64(100000), *result.TokenMaxLimit)
	assert.Equal(t, int64(1000), *result.RequestMaxLimit)
}

func TestCreateRateLimit_InvalidDuration(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)
	invalidDuration := "invalid"

	rateLimit := &tables.TableRateLimit{
		ID:                 "rate-limit-invalid",
		TokenMaxLimit:      &tokenMax,
		TokenResetDuration: &invalidDuration,
	}

	err := store.CreateRateLimit(ctx, rateLimit)
	assert.Error(t, err, "Should fail with invalid duration")
}

func TestCreateRateLimit_MissingDuration(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)

	rateLimit := &tables.TableRateLimit{
		ID:            "rate-limit-missing",
		TokenMaxLimit: &tokenMax,
		// Missing TokenResetDuration
	}

	err := store.CreateRateLimit(ctx, rateLimit)
	assert.Error(t, err, "Should fail when max limit set without duration")
}

func TestUpdateRateLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)
	tokenDuration := "1h"

	rateLimit := &tables.TableRateLimit{
		ID:                 "rate-limit-update",
		TokenMaxLimit:      &tokenMax,
		TokenResetDuration: &tokenDuration,
	}
	err := store.CreateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	// Update
	newMax := int64(200000)
	rateLimit.TokenMaxLimit = &newMax
	err = store.UpdateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	result, err := store.GetRateLimit(ctx, "rate-limit-update")
	require.NoError(t, err)
	assert.Equal(t, int64(200000), *result.TokenMaxLimit)
}

// =============================================================================
// Virtual Key Tests
// =============================================================================

func TestCreateVirtualKey(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-test",
		Name:     "Test Virtual Key",
		Value:    "vk-test-value-123",
		IsActive: schemas.Ptr(true),
	}

	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-test")
	require.NoError(t, err)
	assert.Equal(t, "vk-test", result.ID)
	assert.Equal(t, "Test Virtual Key", result.Name)
	assert.Equal(t, "vk-test-value-123", result.Value)
	assert.True(t, result.IsActiveValue())
}

func TestCreateVirtualKey_WithBudgetAndRateLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create budget first
	budget := &tables.TableBudget{
		ID:            "budget-for-vk",
		MaxLimit:      100.0,
		ResetDuration: "1M",
	}
	err := store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Create rate limit
	tokenMax := int64(100000)
	tokenDuration := "1h"
	rateLimit := &tables.TableRateLimit{
		ID:                 "rate-limit-for-vk",
		TokenMaxLimit:      &tokenMax,
		TokenResetDuration: &tokenDuration,
	}
	err = store.CreateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	// Create virtual key with references
	rateLimitID := "rate-limit-for-vk"
	vkID := "vk-with-refs"
	vk := &tables.TableVirtualKey{
		ID:          vkID,
		Name:        "VK With References",
		Value:       "vk-refs-value",
		IsActive:    schemas.Ptr(true),
		RateLimitID: &rateLimitID,
	}

	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Link the existing budget to the VK via FK
	budget.VirtualKeyID = &vkID
	err = store.UpdateBudget(ctx, budget)
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-with-refs")
	require.NoError(t, err)
	assert.Len(t, result.Budgets, 1)
	assert.Equal(t, "budget-for-vk", result.Budgets[0].ID)
	assert.NotNil(t, result.RateLimitID)
	assert.Equal(t, "rate-limit-for-vk", *result.RateLimitID)
}

func TestCreateVirtualKey_DuplicateName(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk1 := &tables.TableVirtualKey{
		ID:       "vk-1",
		Name:     "Same Name",
		Value:    "vk-value-1",
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk1)
	require.NoError(t, err)

	vk2 := &tables.TableVirtualKey{
		ID:       "vk-2",
		Name:     "Same Name", // Duplicate name
		Value:    "vk-value-2",
		IsActive: schemas.Ptr(true),
	}
	err = store.CreateVirtualKey(ctx, vk2)
	assert.Error(t, err, "Should fail with duplicate name")
}

func TestGetVirtualKeyByValue(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-lookup",
		Name:     "Lookup Key",
		Value:    "vk-unique-value-xyz",
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	result, err := store.GetVirtualKeyByValue(ctx, "vk-unique-value-xyz")
	require.NoError(t, err)
	assert.Equal(t, "vk-lookup", result.ID)
}

func TestUpdateVirtualKey(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-update",
		Name:     "Original Name",
		Value:    "vk-update-value",
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Update
	vk.Name = "Updated Name"
	vk.IsActive = schemas.Ptr(false)
	err = store.UpdateVirtualKey(ctx, vk)
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-update")
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", result.Name)
	assert.False(t, result.IsActiveValue())
}

func TestDeleteVirtualKey(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-delete",
		Name:     "Delete Me",
		Value:    "vk-delete-value",
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	err = store.DeleteVirtualKey(ctx, "vk-delete")
	require.NoError(t, err)

	_, err = store.GetVirtualKey(ctx, "vk-delete")
	assert.Error(t, err, "Should not find deleted virtual key")
}

// =============================================================================
// Virtual Key Provider Config Tests
// =============================================================================

func TestCreateVirtualKeyProviderConfig(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create virtual key first
	vk := &tables.TableVirtualKey{
		ID:       "vk-for-pc",
		Name:     "VK For Provider Config",
		Value:    "vk-pc-value",
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Create provider config
	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-for-pc",
		Provider:     "openai",
		Weight:       &weight,
	}

	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	// Verify
	configs, err := store.GetVirtualKeyProviderConfigs(ctx, "vk-for-pc")
	require.NoError(t, err)
	assert.Len(t, configs, 1)
	assert.Equal(t, "openai", configs[0].Provider)
}

func TestCreateVirtualKeyProviderConfig_WithKeys(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create provider with keys first
	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{ID: "key-for-pc", Name: "openai-pc-key", Value: *schemas.NewEnvVar("sk-test"), Weight: 1.0},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Create virtual key
	vk := &tables.TableVirtualKey{
		ID:       "vk-with-keys",
		Name:     "VK With Keys",
		Value:    "vk-keys-value",
		IsActive: schemas.Ptr(true),
	}
	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Create provider config with key reference
	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-with-keys",
		Provider:     "openai",
		Weight:       &weight,
		Keys: []tables.TableKey{
			{Name: "openai-pc-key"}, // Reference by name
		},
	}

	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	// Verify keys are associated
	configs, err := store.GetVirtualKeyProviderConfigs(ctx, "vk-with-keys")
	require.NoError(t, err)
	assert.Len(t, configs, 1)

	// Load with keys
	var configWithKeys tables.TableVirtualKeyProviderConfig
	err = store.DB().Preload("Keys").First(&configWithKeys, "id = ?", configs[0].ID).Error
	require.NoError(t, err)
	assert.Len(t, configWithKeys.Keys, 1)
}

func TestCreateVirtualKeyProviderConfig_UnresolvedKeys(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create virtual key
	vk := &tables.TableVirtualKey{
		ID:       "vk-unresolved",
		Name:     "VK Unresolved",
		Value:    "vk-unresolved-value",
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Try to create provider config with non-existent key
	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-unresolved",
		Provider:     "openai",
		Weight:       &weight,
		Keys: []tables.TableKey{
			{Name: "non-existent-key"},
		},
	}

	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	assert.Error(t, err, "Should fail with unresolved keys")

	var unresolvedErr *ErrUnresolvedKeys
	assert.ErrorAs(t, err, &unresolvedErr, "Should be ErrUnresolvedKeys")
}

func TestUpdateProvider_RemovesStaleVirtualKeyProviderConfigKeyAssociations(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{ID: "key-a", Name: "openai-key-a", Value: *schemas.NewEnvVar("sk-a"), Weight: 1.0},
				{ID: "key-b", Name: "openai-key-b", Value: *schemas.NewEnvVar("sk-b"), Weight: 1.0},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	vk := &tables.TableVirtualKey{
		ID:       "vk-update-provider-cleanup",
		Name:     "VK Update Provider Cleanup",
		Value:    "vk-update-provider-cleanup-value",
		IsActive: schemas.Ptr(true),
	}
	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-update-provider-cleanup",
		Provider:     "openai",
		Weight:       &weight,
		Keys: []tables.TableKey{
			{Name: "openai-key-b"},
		},
	}
	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	updatedProviderConfig := ProviderConfig{
		Keys: []schemas.Key{
			{ID: "key-a", Name: "openai-key-a", Value: *schemas.NewEnvVar("sk-a"), Weight: 1.0},
		},
	}
	err = store.UpdateProvider(ctx, "openai", updatedProviderConfig)
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-update-provider-cleanup")
	require.NoError(t, err)
	require.Len(t, result.ProviderConfigs, 1)
	assert.Equal(t, "openai", result.ProviderConfigs[0].Provider)
	assert.False(t, result.ProviderConfigs[0].AllowAllKeys)
	assert.Empty(t, result.ProviderConfigs[0].Keys)
}

func TestDeleteProvider_RemovesVirtualKeyProviderConfigs(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{{ID: "key-delete", Name: "openai-key-delete", Value: *schemas.NewEnvVar("sk-delete"), Weight: 1.0}},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	vk := &tables.TableVirtualKey{
		ID:       "vk-delete-provider-cleanup",
		Name:     "VK Delete Provider Cleanup",
		Value:    "vk-delete-provider-cleanup-value",
		IsActive: schemas.Ptr(true),
	}
	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-delete-provider-cleanup",
		Provider:     "openai",
		Weight:       &weight,
	}
	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	err = store.DeleteProvider(ctx, "openai")
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-delete-provider-cleanup")
	require.NoError(t, err)
	assert.Empty(t, result.ProviderConfigs)
}

// =============================================================================
// Client Config Tests
// =============================================================================

func TestUpdateClientConfig(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	config := &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      100,
		LogRetentionDays:     30,
		MaxRequestBodySizeMB: 50,
	}

	err := store.UpdateClientConfig(ctx, config)
	require.NoError(t, err)

	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.True(t, result.EnableLogging != nil && *result.EnableLogging)
	assert.Equal(t, 100, result.InitialPoolSize)
}

func TestUpdateClientMetadata(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateClientConfig(ctx, &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      100,
		LogRetentionDays:     30,
		MaxRequestBodySizeMB: 50,
	})
	require.NoError(t, err)

	err = store.UpdateClientMetadata(ctx, map[string]any{
		"onboarding_dismissed": true,
		"theme":                "dark",
	})
	require.NoError(t, err)

	err = store.UpdateClientMetadata(ctx, map[string]any{
		"theme": "light",
		"stale": nil,
	})
	require.NoError(t, err)

	metadata, err := store.GetClientMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, true, metadata["onboarding_dismissed"])
	assert.Equal(t, "light", metadata["theme"])
	assert.NotContains(t, metadata, "stale")

	err = store.UpdateClientMetadata(ctx, map[string]any{"theme": nil})
	require.NoError(t, err)

	metadata, err = store.GetClientMetadata(ctx)
	require.NoError(t, err)
	assert.NotContains(t, metadata, "theme")
	assert.Equal(t, true, metadata["onboarding_dismissed"])

	// Nested objects must be merged recursively (RFC 7386), not replaced
	// wholesale, so sibling keys survive a partial nested patch.
	err = store.UpdateClientMetadata(ctx, map[string]any{
		"onboarding": map[string]any{"dismissed": true, "step": "a"},
	})
	require.NoError(t, err)

	err = store.UpdateClientMetadata(ctx, map[string]any{
		"onboarding": map[string]any{"step": "b"},
	})
	require.NoError(t, err)

	metadata, err = store.GetClientMetadata(ctx)
	require.NoError(t, err)
	onboarding, ok := metadata["onboarding"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, onboarding["dismissed"], "sibling key must survive nested patch")
	assert.Equal(t, "b", onboarding["step"])

	// A nil nested value deletes just that nested key.
	err = store.UpdateClientMetadata(ctx, map[string]any{
		"onboarding": map[string]any{"dismissed": nil},
	})
	require.NoError(t, err)

	metadata, err = store.GetClientMetadata(ctx)
	require.NoError(t, err)
	onboarding, ok = metadata["onboarding"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, onboarding, "dismissed")
	assert.Equal(t, "b", onboarding["step"])
}

func TestUpdateClientMetadataRequiresClientConfig(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateClientMetadata(ctx, map[string]any{"onboarding_dismissed": true})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotFound)

	var count int64
	err = store.DB().WithContext(ctx).Model(&tables.TableClientConfig{}).Count(&count).Error
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestUpdateClientConfigPreservesMetadata(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateClientConfig(ctx, &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      100,
		LogRetentionDays:     30,
		MaxRequestBodySizeMB: 50,
	})
	require.NoError(t, err)

	err = store.UpdateClientMetadata(ctx, map[string]any{"onboarding_dismissed": true})
	require.NoError(t, err)

	err = store.UpdateClientConfig(ctx, &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      200,
		LogRetentionDays:     60,
		MaxRequestBodySizeMB: 100,
	})
	require.NoError(t, err)

	config, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, 200, config.InitialPoolSize)

	metadata, err := store.GetClientMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, true, metadata["onboarding_dismissed"])
}

// =============================================================================
// Transaction Tests
// =============================================================================

func TestExecuteTransaction_Success(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		// Create budget in transaction
		budget := &tables.TableBudget{
			ID:            "tx-budget",
			MaxLimit:      100.0,
			ResetDuration: "1h",
		}
		return tx.Create(budget).Error
	})
	require.NoError(t, err)

	// Verify budget was created
	result, err := store.GetBudget(ctx, "tx-budget")
	require.NoError(t, err)
	assert.Equal(t, "tx-budget", result.ID)
}

func TestExecuteTransaction_Rollback(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		// Create budget
		budget := &tables.TableBudget{
			ID:            "tx-rollback-budget",
			MaxLimit:      100.0,
			ResetDuration: "1h",
		}
		if err := tx.Create(budget).Error; err != nil {
			return err
		}

		// Force error to trigger rollback
		return assert.AnError
	})
	assert.Error(t, err)

	// Verify budget was NOT created (rolled back)
	_, err = store.GetBudget(ctx, "tx-rollback-budget")
	assert.Error(t, err, "Budget should not exist after rollback")
}

// =============================================================================
// Customer and Team Tests
// =============================================================================

func TestCreateCustomer(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	customer := &tables.TableCustomer{
		ID:   "customer-test",
		Name: "Test Customer",
	}

	err := store.CreateCustomer(ctx, customer)
	require.NoError(t, err)

	result, err := store.GetCustomer(ctx, "customer-test")
	require.NoError(t, err)
	assert.Equal(t, "Test Customer", result.Name)
}

func TestCreateTeam(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create customer first
	customer := &tables.TableCustomer{
		ID:   "customer-for-team",
		Name: "Customer For Team",
	}
	err := store.CreateCustomer(ctx, customer)
	require.NoError(t, err)

	// Create team
	customerID := "customer-for-team"
	team := &tables.TableTeam{
		ID:         "team-test",
		Name:       "Test Team",
		CustomerID: &customerID,
	}

	err = store.CreateTeam(ctx, team)
	require.NoError(t, err)

	result, err := store.GetTeam(ctx, "team-test")
	require.NoError(t, err)
	assert.Equal(t, "Test Team", result.Name)
	assert.Equal(t, "customer-for-team", *result.CustomerID)
}

// =============================================================================
// Tenant CRUD Tests
// =============================================================================

func TestTenant_CreateGetUpdate(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Default tenant is seeded by the initial multi-tenant migration.
	def, err := store.GetTenant(ctx, tables.DefaultTenantID)
	require.NoError(t, err)
	assert.Equal(t, tables.DefaultTenantID, def.ID)
	assert.Equal(t, tables.TenantStatusActive, def.Status)

	// Create.
	tenant := &tables.TableTenant{
		ID:          "acme",
		Name:        "Acme Corp",
		Status:      tables.TenantStatusActive,
		Description: "primary acme account",
	}
	require.NoError(t, store.CreateTenant(ctx, tenant))

	got, err := store.GetTenant(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, "Acme Corp", got.Name)
	assert.Equal(t, "primary acme account", got.Description)

	// Update — flip to suspended + rename.
	got.Status = tables.TenantStatusSuspended
	got.Name = "Acme Corp (suspended)"
	require.NoError(t, store.UpdateTenant(ctx, got))

	got2, err := store.GetTenant(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, tables.TenantStatusSuspended, got2.Status)
	assert.Equal(t, "Acme Corp (suspended)", got2.Name)
}

func TestTenant_CreateDuplicateRejected(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tenant := &tables.TableTenant{
		ID:     "acme",
		Name:   "Acme",
		Status: tables.TenantStatusActive,
	}
	require.NoError(t, store.CreateTenant(ctx, tenant))

	// Re-inserting the same ID must fail. parseGormError typically maps
	// uniqueness violations to ErrAlreadyExists; the exact sentinel is
	// less important than the error not being nil.
	err := store.CreateTenant(ctx, tenant)
	require.Error(t, err)
}

func TestTenant_GetNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	_, err := store.GetTenant(ctx, "no-such-tenant")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestTenant_DeleteEmptyTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tenant := &tables.TableTenant{
		ID:     "empty-tenant",
		Name:   "Empty",
		Status: tables.TenantStatusActive,
	}
	require.NoError(t, store.CreateTenant(ctx, tenant))
	require.NoError(t, store.DeleteTenant(ctx, "empty-tenant"))

	_, err := store.GetTenant(ctx, "empty-tenant")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestTenant_DeleteRefusedWhenNotEmpty(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tenant := &tables.TableTenant{
		ID:     "owns-stuff",
		Name:   "Owns Stuff",
		Status: tables.TenantStatusActive,
	}
	require.NoError(t, store.CreateTenant(ctx, tenant))

	// Insert a customer assigned to this tenant.
	customer := &tables.TableCustomer{
		ID:       "owned-customer",
		Name:     "Owned Customer",
		TenantID: "owns-stuff",
	}
	require.NoError(t, store.CreateCustomer(ctx, customer))

	err := store.DeleteTenant(ctx, "owns-stuff")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTenantNotEmpty)

	// Tenant must still exist.
	_, err = store.GetTenant(ctx, "owns-stuff")
	require.NoError(t, err)

	// Remove the customer; delete now succeeds.
	require.NoError(t, store.DeleteCustomer(ctx, "owned-customer"))
	require.NoError(t, store.DeleteTenant(ctx, "owns-stuff"))
}

func TestTenant_DeleteDefaultRefused(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.DeleteTenant(ctx, tables.DefaultTenantID)
	assert.ErrorIs(t, err, ErrReservedTenant)

	// Default tenant is still there.
	def, err := store.GetTenant(ctx, tables.DefaultTenantID)
	require.NoError(t, err)
	assert.Equal(t, tables.DefaultTenantID, def.ID)
}

func TestTenant_ListAndPaginate(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Seed three tenants beyond the default.
	for _, id := range []string{"alpha", "beta", "gamma"} {
		require.NoError(t, store.CreateTenant(ctx, &tables.TableTenant{
			ID:     id,
			Name:   strings.ToUpper(id),
			Status: tables.TenantStatusActive,
		}))
	}

	all, err := store.GetTenants(ctx)
	require.NoError(t, err)
	// At minimum: default + 3 we just added.
	assert.GreaterOrEqual(t, len(all), 4)

	// Search by lowercased name fragment.
	page, total, err := store.GetTenantsPaginated(ctx, TenantsQueryParams{Search: "alph", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, page, 1)
	assert.Equal(t, "alpha", page[0].ID)

	// Filter by status — flip beta to suspended.
	beta, err := store.GetTenant(ctx, "beta")
	require.NoError(t, err)
	beta.Status = tables.TenantStatusSuspended
	require.NoError(t, store.UpdateTenant(ctx, beta))

	page, _, err = store.GetTenantsPaginated(ctx, TenantsQueryParams{
		Status: tables.TenantStatusSuspended,
		Limit:  10,
	})
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "beta", page[0].ID)
}

// =============================================================================
// Ping and Health Tests
// =============================================================================

func TestPing(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.Ping(ctx)
	assert.NoError(t, err)
}

// =============================================================================
// Error Handling Tests
// =============================================================================

func TestGetBudget_NotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	_, err := store.GetBudget(ctx, "non-existent-budget")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetVirtualKey_NotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	_, err := store.GetVirtualKey(ctx, "non-existent-vk")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetRateLimit_NotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	_, err := store.GetRateLimit(ctx, "non-existent-rate-limit")
	assert.ErrorIs(t, err, ErrNotFound)
}

// =============================================================================
// Plugin Tests
// =============================================================================

func TestCreateAndGetPlugin(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	plugin := &tables.TablePlugin{
		Name:    "test-plugin",
		Enabled: true,
		Version: 1,
	}

	err := store.CreatePlugin(ctx, plugin)
	require.NoError(t, err)

	result, err := store.GetPlugin(ctx, "test-plugin")
	require.NoError(t, err)
	assert.Equal(t, "test-plugin", result.Name)
	assert.True(t, result.Enabled)
}

func TestUpsertPlugin(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create plugin
	plugin := &tables.TablePlugin{
		Name:    "upsert-plugin",
		Enabled: true,
		Version: 1,
	}
	err := store.UpsertPlugin(ctx, plugin)
	require.NoError(t, err)

	// Upsert with update
	plugin.Version = 2
	err = store.UpsertPlugin(ctx, plugin)
	require.NoError(t, err)

	result, err := store.GetPlugin(ctx, "upsert-plugin")
	require.NoError(t, err)
	assert.Equal(t, int16(2), result.Version)
}

// =============================================================================
// Integration Test: Full Virtual Key with Provider Config Flow
// =============================================================================

func TestFullVirtualKeyFlow(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Step 1: Create provider with keys
	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{ID: "key-1", Name: "openai-main", Value: *schemas.NewEnvVar("sk-main"), Weight: 1.0},
				{ID: "key-2", Name: "openai-backup", Value: *schemas.NewEnvVar("sk-backup"), Weight: 0.5},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Step 2: Create budget
	budget := &tables.TableBudget{
		ID:            "integration-budget",
		MaxLimit:      500.0,
		ResetDuration: "1M",
	}
	err = store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Step 3: Create rate limit
	tokenMax := int64(1000000)
	tokenDuration := "1d"
	rateLimit := &tables.TableRateLimit{
		ID:                 "integration-rate-limit",
		TokenMaxLimit:      &tokenMax,
		TokenResetDuration: &tokenDuration,
	}
	err = store.CreateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	// Step 4: Create virtual key
	rateLimitID := "integration-rate-limit"
	integrationVKID := "integration-vk"
	vk := &tables.TableVirtualKey{
		ID:          integrationVKID,
		Name:        "Integration Virtual Key",
		Value:       "vk-integration-xyz",
		IsActive:    schemas.Ptr(true),
		RateLimitID: &rateLimitID,
	}
	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Link the existing budget to the VK via FK
	budget.VirtualKeyID = &integrationVKID
	err = store.UpdateBudget(ctx, budget)
	require.NoError(t, err)

	// Step 5: Create provider config with key reference
	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "integration-vk",
		Provider:     "openai",
		Weight:       &weight,
		Keys: []tables.TableKey{
			{Name: "openai-main"},
		},
	}
	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	// Step 6: Verify complete setup
	result, err := store.GetVirtualKey(ctx, "integration-vk")
	require.NoError(t, err)
	assert.Equal(t, "Integration Virtual Key", result.Name)
	assert.Len(t, result.Budgets, 1)
	assert.NotNil(t, result.RateLimitID)

	configs, err := store.GetVirtualKeyProviderConfigs(ctx, "integration-vk")
	require.NoError(t, err)
	assert.Len(t, configs, 1)
	assert.Equal(t, "openai", configs[0].Provider)
}

// =============================================================================
// Helper function tests
// =============================================================================

func TestGetWeight(t *testing.T) {
	// Test nil weight returns default
	assert.Equal(t, 1.0, getWeight(nil))

	// Test explicit weight
	w := 2.5
	assert.Equal(t, 2.5, getWeight(&w))

	// Test zero weight
	zero := 0.0
	assert.Equal(t, 0.0, getWeight(&zero))
}

// =============================================================================
// Concurrent Access Tests
// =============================================================================

func TestMultipleBudgetUpdates(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create initial budget
	budget := &tables.TableBudget{
		ID:            "multi-update-budget",
		MaxLimit:      100.0,
		ResetDuration: "1h",
		CurrentUsage:  0,
	}
	err := store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Simulate multiple sequential updates
	for i := 0; i < 10; i++ {
		b := &tables.TableBudget{
			ID:            "multi-update-budget",
			MaxLimit:      100.0 + float64(i),
			ResetDuration: "1h",
		}
		err := store.UpdateBudget(ctx, b)
		require.NoError(t, err)
	}

	// Verify budget exists and has the last value
	result, err := store.GetBudget(ctx, "multi-update-budget")
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, 109.0, result.MaxLimit) // 100 + 9
}

// =============================================================================
// Duration Validation Tests (for budgets and rate limits)
// =============================================================================

func TestBudgetDurationFormats(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	validDurations := []string{"30s", "5m", "1h", "1d", "1w", "1M", "1Y"}

	for i, duration := range validDurations {
		budget := &tables.TableBudget{
			ID:            "budget-duration-" + string(rune('a'+i)),
			MaxLimit:      100.0,
			ResetDuration: duration,
		}
		err := store.CreateBudget(ctx, budget)
		assert.NoError(t, err, "Duration %s should be valid", duration)
	}
}

func TestRateLimitDurationFormats(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	validDurations := []string{"30s", "5m", "1h", "1d", "1w", "1M", "1Y"}

	for i, duration := range validDurations {
		tokenMax := int64(1000)
		rateLimit := &tables.TableRateLimit{
			ID:                 "rate-limit-duration-" + string(rune('a'+i)),
			TokenMaxLimit:      &tokenMax,
			TokenResetDuration: &duration,
		}
		err := store.CreateRateLimit(ctx, rateLimit)
		assert.NoError(t, err, "Duration %s should be valid", duration)
	}
}

// =============================================================================
// Prompt Deletion Tests
// =============================================================================

// testPromptTree holds IDs of entities created by createTestPromptTree for verification
type testPromptTree struct {
	FolderID   string
	PromptIDs  []string
	VersionIDs []uint
	SessionIDs []uint
}

// createTestPromptTree creates a folder with 2 prompts, each having 2 versions (with messages) and 1 session (with messages).
func createTestPromptTree(t *testing.T, store *RDBConfigStore, ctx context.Context) testPromptTree {
	t.Helper()

	tree := testPromptTree{}

	// Create folder
	folder := &tables.TableFolder{ID: "folder-1", Name: "Test Folder"}
	require.NoError(t, store.CreateFolder(ctx, folder))
	tree.FolderID = folder.ID

	for i, promptID := range []string{"prompt-1", "prompt-2"} {
		_ = i
		prompt := &tables.TablePrompt{ID: promptID, Name: "Prompt " + promptID, FolderID: &tree.FolderID}
		require.NoError(t, store.CreatePrompt(ctx, prompt))
		tree.PromptIDs = append(tree.PromptIDs, promptID)

		// Create 2 versions with messages
		for v := 0; v < 2; v++ {
			version := &tables.TablePromptVersion{
				PromptID:      promptID,
				CommitMessage: "version commit",
				Messages: []tables.TablePromptVersionMessage{
					{PromptID: promptID, Message: json.RawMessage(`{"role":"user","content":"hello"}`)},
				},
			}
			require.NoError(t, store.CreatePromptVersion(ctx, version))
			tree.VersionIDs = append(tree.VersionIDs, version.ID)
		}

		// Create 1 session with messages
		session := &tables.TablePromptSession{
			PromptID: promptID,
			Name:     "Session " + promptID,
			Messages: []tables.TablePromptSessionMessage{
				{PromptID: promptID, Message: json.RawMessage(`{"role":"user","content":"hi"}`)},
			},
		}
		require.NoError(t, store.CreatePromptSession(ctx, session))
		tree.SessionIDs = append(tree.SessionIDs, session.ID)
	}

	return tree
}

// countRows returns the number of rows in a table
func countRows(t *testing.T, store *RDBConfigStore, model interface{}) int64 {
	t.Helper()
	var count int64
	require.NoError(t, store.DB().Model(model).Count(&count).Error)
	return count
}

func TestDeleteFolder(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		err := store.DeleteFolder(ctx, "nonexistent")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Empty", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		folder := &tables.TableFolder{ID: "folder-empty", Name: "Empty"}
		require.NoError(t, store.CreateFolder(ctx, folder))

		require.NoError(t, store.DeleteFolder(ctx, "folder-empty"))

		_, err := store.GetFolderByID(ctx, "folder-empty")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("CascadesAll", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		// Verify entities exist before deletion
		assert.Greater(t, countRows(t, store, &tables.TablePrompt{}), int64(0))
		assert.Greater(t, countRows(t, store, &tables.TablePromptVersion{}), int64(0))
		assert.Greater(t, countRows(t, store, &tables.TablePromptVersionMessage{}), int64(0))
		assert.Greater(t, countRows(t, store, &tables.TablePromptSession{}), int64(0))
		assert.Greater(t, countRows(t, store, &tables.TablePromptSessionMessage{}), int64(0))

		require.NoError(t, store.DeleteFolder(ctx, tree.FolderID))

		// All child entities should be deleted
		assert.Equal(t, int64(0), countRows(t, store, &tables.TableFolder{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePrompt{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePromptVersion{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePromptVersionMessage{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePromptSession{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePromptSessionMessage{}))
	})
}

func TestDeletePrompt(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		err := store.DeletePrompt(ctx, "nonexistent")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("CascadesAll", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePrompt(ctx, tree.PromptIDs[0]))

		// First prompt and its children should be gone
		_, err := store.GetPromptByID(ctx, tree.PromptIDs[0])
		assert.ErrorIs(t, err, ErrNotFound)

		// Second prompt should still exist
		p2, err := store.GetPromptByID(ctx, tree.PromptIDs[1])
		require.NoError(t, err)
		assert.Equal(t, tree.PromptIDs[1], p2.ID)
	})

	t.Run("LeavesFolder", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePrompt(ctx, tree.PromptIDs[0]))

		// Folder should still exist
		folder, err := store.GetFolderByID(ctx, tree.FolderID)
		require.NoError(t, err)
		assert.Equal(t, tree.FolderID, folder.ID)
	})

	t.Run("LeavesSiblings", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePrompt(ctx, tree.PromptIDs[0]))

		// Sibling prompt's versions and sessions should be unaffected
		versions, err := store.GetPromptVersions(ctx, tree.PromptIDs[1])
		require.NoError(t, err)
		assert.Len(t, versions, 2)

		sessions, err := store.GetPromptSessions(ctx, tree.PromptIDs[1])
		require.NoError(t, err)
		assert.Len(t, sessions, 1)
	})
}

func TestDeletePromptVersion(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		err := store.DeletePromptVersion(ctx, 99999)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("NonLatest", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		// Version at index 0 is v1 (non-latest), index 1 is v2 (latest) for prompt-1
		nonLatestID := tree.VersionIDs[0]
		latestID := tree.VersionIDs[1]

		require.NoError(t, store.DeletePromptVersion(ctx, nonLatestID))

		// Non-latest version should be gone
		_, err := store.GetPromptVersionByID(ctx, nonLatestID)
		assert.ErrorIs(t, err, ErrNotFound)

		// Latest version should still be latest
		latest, err := store.GetPromptVersionByID(ctx, latestID)
		require.NoError(t, err)
		assert.True(t, latest.IsLatest)
	})

	t.Run("LatestPromotesPrevious", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		// Delete the latest version (index 1 = v2 for prompt-1)
		latestID := tree.VersionIDs[1]
		prevID := tree.VersionIDs[0]

		require.NoError(t, store.DeletePromptVersion(ctx, latestID))

		// Previous version should now be latest
		prev, err := store.GetPromptVersionByID(ctx, prevID)
		require.NoError(t, err)
		assert.True(t, prev.IsLatest)
	})

	t.Run("LeavesPrompt", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePromptVersion(ctx, tree.VersionIDs[0]))

		// Prompt should still exist
		prompt, err := store.GetPromptByID(ctx, tree.PromptIDs[0])
		require.NoError(t, err)
		assert.Equal(t, tree.PromptIDs[0], prompt.ID)
	})
}

func TestDeletePromptSession(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		err := store.DeletePromptSession(ctx, 99999)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("CascadesMessages", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		sessionID := tree.SessionIDs[0]
		require.NoError(t, store.DeletePromptSession(ctx, sessionID))

		// Session should be gone
		_, err := store.GetPromptSessionByID(ctx, sessionID)
		assert.ErrorIs(t, err, ErrNotFound)

		// Session messages for that session should be gone
		var msgCount int64
		require.NoError(t, store.DB().Model(&tables.TablePromptSessionMessage{}).Where("session_id = ?", sessionID).Count(&msgCount).Error)
		assert.Equal(t, int64(0), msgCount)
	})

	t.Run("LeavesPrompt", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePromptSession(ctx, tree.SessionIDs[0]))

		// Prompt and versions should still exist
		prompt, err := store.GetPromptByID(ctx, tree.PromptIDs[0])
		require.NoError(t, err)
		assert.Equal(t, tree.PromptIDs[0], prompt.ID)

		versions, err := store.GetPromptVersions(ctx, tree.PromptIDs[0])
		require.NoError(t, err)
		assert.Len(t, versions, 2)
	})
}

// =============================================================================
// GetProvidersConfigByTenant
// =============================================================================

// TestGetProvidersConfigByTenant_IsolatesProvidersBetweenTenants verifies
// that two tenants with overlapping provider Names see disjoint provider
// configs when queried by tenant_id. Patches 0007–0010 made (tenant_id,
// name) the composite-unique key on config_providers; this test confirms
// the by-tenant query honours that.
func TestGetProvidersConfigByTenant_IsolatesProvidersBetweenTenants(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Two tenants both register an "openai" provider.
	for _, p := range []*tables.TableProvider{
		{Name: "openai", TenantID: "tenant-a"},
		{Name: "openai", TenantID: "tenant-b"},
	} {
		require.NoError(t, store.DB().WithContext(ctx).Create(p).Error)
	}

	a, err := store.GetProvidersConfigByTenant(ctx, "tenant-a")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Len(t, a, 1, "tenant-a should only see its own openai")

	b, err := store.GetProvidersConfigByTenant(ctx, "tenant-b")
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.Len(t, b, 1, "tenant-b should only see its own openai")

	// GetProvidersConfig (no tenant filter) still returns both — it's the
	// global accessor used by legacy single-tenant boot paths.
	all, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1, "global Get returns 1 entry because the map key is provider name; both rows collapse to one entry")
}

func TestGetProvidersConfigByTenant_EmptyTenantNormalizesToDefault(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Insert a provider explicitly assigned to the default tenant.
	require.NoError(t, store.DB().WithContext(ctx).Create(&tables.TableProvider{
		Name:     "openai",
		TenantID: tables.DefaultTenantID,
	}).Error)

	// Querying with an empty tenant id should resolve to the default tenant
	// and return its row, so single-tenant callers that haven't been
	// migrated to pass an explicit tenant id keep working.
	got, err := store.GetProvidersConfigByTenant(ctx, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
}

// TestAddProviderForTenant_WritesTenantID asserts the new mutator pins the
// supplied tenant_id on the created TableProvider row (and on its keys)
// rather than relying on the column default, so the resulting provider is
// only visible to GetProvidersConfigByTenant for that tenant.
func TestAddProviderForTenant_WritesTenantID(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	concurrency := 4
	bufSize := 16
	cfg := ProviderConfig{
		Keys: []schemas.Key{
			{ID: "key-acme-1", Name: "primary", Value: *schemas.NewEnvVar("sk-secret-acme")},
		},
		ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: concurrency, BufferSize: bufSize},
	}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", cfg))

	acme, err := store.GetProvidersConfigByTenant(ctx, "acme")
	require.NoError(t, err)
	require.Len(t, acme, 1, "acme should see its own openai")
	require.Len(t, acme["openai"].Keys, 1, "tenant-scoped read should include the key created with the provider")

	def, err := store.GetProvidersConfigByTenant(ctx, tables.DefaultTenantID)
	require.NoError(t, err)
	assert.Empty(t, def, "default tenant must not see acme's provider")
}

// TestAddProviderForTenant_TwoTenantsSameName confirms (tenant_id, name)
// composite uniqueness lets two tenants both register a provider named
// "openai" without colliding, and reads stay isolated.
func TestAddProviderForTenant_TwoTenantsSameName(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	concurrency := 1
	bufSize := 4
	makeCfg := func(keyValue string) ProviderConfig {
		return ProviderConfig{
			Keys:                     []schemas.Key{{ID: keyValue, Name: keyValue, Value: *schemas.NewEnvVar(keyValue)}},
			ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: concurrency, BufferSize: bufSize},
		}
	}

	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", makeCfg("acme-key")))
	require.NoError(t, store.AddProviderForTenant(ctx, "globex", "openai", makeCfg("globex-key")))

	acme, err := store.GetProvidersConfigByTenant(ctx, "acme")
	require.NoError(t, err)
	require.Len(t, acme["openai"].Keys, 1)
	assert.Equal(t, "acme-key", acme["openai"].Keys[0].ID, "acme's key must not bleed into globex")

	globex, err := store.GetProvidersConfigByTenant(ctx, "globex")
	require.NoError(t, err)
	require.Len(t, globex["openai"].Keys, 1)
	assert.Equal(t, "globex-key", globex["openai"].Keys[0].ID, "globex's key must not bleed into acme")
}

// TestAddProviderForTenant_EmptyTenantNormalizesToDefault confirms that
// callers passing "" (e.g. a single-tenant deployment that hasn't been
// migrated to thread tenant_id yet) land on the seeded default tenant,
// matching GetProvidersConfigByTenant's normalisation.
func TestAddProviderForTenant_EmptyTenantNormalizesToDefault(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	cfg := ProviderConfig{Keys: []schemas.Key{{ID: "k1", Value: *schemas.NewEnvVar("sk-x")}}}
	require.NoError(t, store.AddProviderForTenant(ctx, "", "openai", cfg))

	def, err := store.GetProvidersConfigByTenant(ctx, tables.DefaultTenantID)
	require.NoError(t, err)
	require.Len(t, def, 1, "empty tenant id should land in the default tenant scope")
}

// TestCreateMCPClientConfigForTenant_WritesTenantID confirms the new
// mutator pins tenant_id on the new TableMCPClient row and that
// GetMCPConfigByTenant honors the filter.
func TestCreateMCPClientConfigForTenant_WritesTenantID(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	mcp := &schemas.MCPClientConfig{
		ID:               "mcp-acme-1",
		Name:             "tools",
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewEnvVar("https://acme.example.com/mcp"),
	}
	require.NoError(t, store.CreateMCPClientConfigForTenant(ctx, "acme", mcp))

	acme, err := store.GetMCPConfigByTenant(ctx, "acme")
	require.NoError(t, err)
	require.NotNil(t, acme)
	require.Len(t, acme.ClientConfigs, 1, "acme should see its own MCP client")
	assert.Equal(t, "tools", acme.ClientConfigs[0].Name)

	def, err := store.GetMCPConfigByTenant(ctx, tables.DefaultTenantID)
	require.NoError(t, err)
	if def != nil {
		assert.Empty(t, def.ClientConfigs, "default tenant must not see acme's MCP client")
	}
}

// TestGetProviderConfigByTenant_IsolatesByTenant confirms two tenants
// with overlapping provider names get back their own row.
func TestGetProviderConfigByTenant_IsolatesByTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	makeCfg := func(keyID string) ProviderConfig {
		return ProviderConfig{
			Keys: []schemas.Key{{ID: keyID, Value: *schemas.NewEnvVar(keyID)}},
		}
	}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", makeCfg("acme-key")))
	require.NoError(t, store.AddProviderForTenant(ctx, "globex", "openai", makeCfg("globex-key")))

	got, err := store.GetProviderConfigByTenant(ctx, "acme", "openai")
	require.NoError(t, err)
	require.Len(t, got.Keys, 1)
	assert.Equal(t, "acme-key", got.Keys[0].ID)

	got, err = store.GetProviderConfigByTenant(ctx, "globex", "openai")
	require.NoError(t, err)
	require.Len(t, got.Keys, 1)
	assert.Equal(t, "globex-key", got.Keys[0].ID)
}

// TestGetProviderConfigByTenant_OtherTenantNotFound asserts that a
// tenant's get for a provider name that exists in another tenant's
// scope returns ErrNotFound, not the other tenant's row.
func TestGetProviderConfigByTenant_OtherTenantNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	cfg := ProviderConfig{Keys: []schemas.Key{{ID: "k", Value: *schemas.NewEnvVar("sk-x")}}}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", cfg))

	_, err := store.GetProviderConfigByTenant(ctx, "globex", "openai")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestDeleteProviderForTenant_OnlyRemovesOwnTenant confirms deleting
// "openai" for tenant acme leaves globex's "openai" intact. Uses
// distinct key IDs because TableKey.KeyID is globally unique (it is
// referenced by OauthUserToken FKs).
func TestDeleteProviderForTenant_OnlyRemovesOwnTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	makeCfg := func(keyID string) ProviderConfig {
		return ProviderConfig{
			Keys: []schemas.Key{{ID: keyID, Value: *schemas.NewEnvVar(keyID)}},
		}
	}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", makeCfg("acme-k")))
	require.NoError(t, store.AddProviderForTenant(ctx, "globex", "openai", makeCfg("globex-k")))

	require.NoError(t, store.DeleteProviderForTenant(ctx, "acme", "openai"))

	_, err := store.GetProviderConfigByTenant(ctx, "acme", "openai")
	assert.ErrorIs(t, err, ErrNotFound, "acme should no longer have openai")

	_, err = store.GetProviderConfigByTenant(ctx, "globex", "openai")
	assert.NoError(t, err, "globex's openai must survive acme's delete")
}

// TestDeleteProviderForTenant_NotFoundInThisTenant confirms that
// deleting a provider that exists in another tenant's scope returns
// ErrNotFound rather than silently deleting cross-tenant.
func TestDeleteProviderForTenant_NotFoundInThisTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	cfg := ProviderConfig{Keys: []schemas.Key{{ID: "k", Value: *schemas.NewEnvVar("sk-x")}}}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", cfg))

	err := store.DeleteProviderForTenant(ctx, "globex", "openai")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestCreateProviderKeyForTenant_PinsTenantID confirms a key added via
// the tenant-scoped mutator stays visible only within that tenant's
// scope. Uses distinct KeyIDs because TableKey.KeyID is globally
// unique.
func TestCreateProviderKeyForTenant_PinsTenantID(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	makeCfg := func(keyID string) ProviderConfig {
		return ProviderConfig{
			Keys: []schemas.Key{{ID: keyID, Name: keyID, Value: *schemas.NewEnvVar(keyID)}},
		}
	}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", makeCfg("acme-initial")))
	require.NoError(t, store.AddProviderForTenant(ctx, "globex", "openai", makeCfg("globex-initial")))

	require.NoError(t, store.CreateProviderKeyForTenant(ctx, "acme", "openai", schemas.Key{
		ID: "acme-rotated", Name: "acme-rotated", Value: *schemas.NewEnvVar("sk-acme-rotated"),
	}))

	// acme should see two keys; globex still only its original one.
	acmeKeys, err := store.GetProviderKeysForTenant(ctx, "acme", "openai")
	require.NoError(t, err)
	assert.Len(t, acmeKeys, 2, "acme should see initial + rotated")

	globexKeys, err := store.GetProviderKeysForTenant(ctx, "globex", "openai")
	require.NoError(t, err)
	assert.Len(t, globexKeys, 1, "globex must not see acme's rotated key")
}

// TestCreateProviderKeyForTenant_CrossTenantProviderNotFound confirms
// adding a key to a provider that lives under a different tenant
// returns ErrNotFound instead of silently writing to the wrong scope.
func TestCreateProviderKeyForTenant_CrossTenantProviderNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	cfg := ProviderConfig{Keys: []schemas.Key{{ID: "k", Name: "k", Value: *schemas.NewEnvVar("sk-x")}}}
	require.NoError(t, store.AddProviderForTenant(ctx, "globex", "openai", cfg))

	err := store.CreateProviderKeyForTenant(ctx, "acme", "openai", schemas.Key{
		ID: "wrong-tenant", Name: "wrong-tenant", Value: *schemas.NewEnvVar("sk-x"),
	})
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestGetProviderKeyForTenant_CrossTenantReturnsNotFound asserts looking
// up a key whose tenant_id is different from the caller's returns
// ErrNotFound rather than the foreign key's contents.
func TestGetProviderKeyForTenant_CrossTenantReturnsNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	cfg := ProviderConfig{Keys: []schemas.Key{{ID: "acme-secret", Name: "acme-secret", Value: *schemas.NewEnvVar("sk-acme")}}}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", cfg))

	got, err := store.GetProviderKeyForTenant(ctx, "acme", "openai", "acme-secret")
	require.NoError(t, err)
	require.NotNil(t, got)

	_, err = store.GetProviderKeyForTenant(ctx, "globex", "openai", "acme-secret")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestDeleteProviderKeyForTenant_OnlyOwnTenant confirms cross-tenant
// delete attempts are refused with ErrNotFound and the target row
// survives.
func TestDeleteProviderKeyForTenant_OnlyOwnTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	for _, p := range []struct{ tid, keyID string }{
		{"acme", "acme-key"},
		{"globex", "globex-key"},
	} {
		require.NoError(t, store.AddProviderForTenant(ctx, p.tid, "openai", ProviderConfig{
			Keys: []schemas.Key{{ID: p.keyID, Name: p.keyID, Value: *schemas.NewEnvVar(p.keyID)}},
		}))
	}

	// acme trying to delete globex's key → ErrNotFound; target survives.
	err := store.DeleteProviderKeyForTenant(ctx, "acme", "openai", "globex-key")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetProviderKeyForTenant(ctx, "globex", "openai", "globex-key")
	require.NoError(t, err)

	// Same-tenant delete succeeds.
	require.NoError(t, store.DeleteProviderKeyForTenant(ctx, "acme", "openai", "acme-key"))
	_, err = store.GetProviderKeyForTenant(ctx, "acme", "openai", "acme-key")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestGetVirtualKeysByTenant_IsolatesByTenant confirms list-by-tenant
// returns only the calling tenant's VKs.
func TestGetVirtualKeysByTenant_IsolatesByTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	for _, vk := range []*tables.TableVirtualKey{
		{ID: "vk-acme-1", Name: "a1", Value: "sk-bf-acme-1", TenantID: "acme"},
		{ID: "vk-acme-2", Name: "a2", Value: "sk-bf-acme-2", TenantID: "acme"},
		{ID: "vk-globex-1", Name: "g1", Value: "sk-bf-globex-1", TenantID: "globex"},
	} {
		require.NoError(t, store.CreateVirtualKey(ctx, vk))
	}

	acme, err := store.GetVirtualKeysByTenant(ctx, "acme")
	require.NoError(t, err)
	assert.Len(t, acme, 2)

	globex, err := store.GetVirtualKeysByTenant(ctx, "globex")
	require.NoError(t, err)
	assert.Len(t, globex, 1)
}

// TestGetVirtualKeyByIDForTenant_CrossTenantReturnsNotFound asserts a
// caller looking up a VK that exists under another tenant gets
// ErrNotFound rather than the other tenant's row.
func TestGetVirtualKeyByIDForTenant_CrossTenantReturnsNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateVirtualKey(ctx, &tables.TableVirtualKey{
		ID: "vk-secret", Name: "secret", Value: "sk-bf-secret", TenantID: "acme",
	}))

	got, err := store.GetVirtualKeyByIDForTenant(ctx, "acme", "vk-secret")
	require.NoError(t, err)
	require.NotNil(t, got)

	_, err = store.GetVirtualKeyByIDForTenant(ctx, "globex", "vk-secret")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestDeleteVirtualKeyForTenant_OnlyOwnTenant confirms the tenant-scoped
// delete refuses cross-tenant probes and only removes within scope.
func TestDeleteVirtualKeyForTenant_OnlyOwnTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateVirtualKey(ctx, &tables.TableVirtualKey{
		ID: "vk-acme", Name: "v-acme", Value: "sk-bf-acme", TenantID: "acme",
	}))
	require.NoError(t, store.CreateVirtualKey(ctx, &tables.TableVirtualKey{
		ID: "vk-globex", Name: "v-globex", Value: "sk-bf-globex", TenantID: "globex",
	}))

	// Cross-tenant attempt -> ErrNotFound, target row survives.
	err := store.DeleteVirtualKeyForTenant(ctx, "acme", "vk-globex")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetVirtualKeyByIDForTenant(ctx, "globex", "vk-globex")
	require.NoError(t, err)

	// Same-tenant delete succeeds.
	require.NoError(t, store.DeleteVirtualKeyForTenant(ctx, "acme", "vk-acme"))
	_, err = store.GetVirtualKeyByIDForTenant(ctx, "acme", "vk-acme")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestBudgetsCRUDForTenant covers create/list/get/update/delete on
// the tenant-scoped budget CRUD with cross-tenant isolation.
func TestBudgetsCRUDForTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateBudgetForTenant(ctx, "acme", &tables.TableBudget{ID: "b-a", MaxLimit: 100, ResetDuration: "1d"}))
	require.NoError(t, store.CreateBudgetForTenant(ctx, "globex", &tables.TableBudget{ID: "b-g", MaxLimit: 200, ResetDuration: "1d"}))

	acme, err := store.GetBudgetsByTenant(ctx, "acme")
	require.NoError(t, err)
	assert.Len(t, acme, 1)
	assert.Equal(t, float64(100), acme[0].MaxLimit)

	_, err = store.GetBudgetForTenant(ctx, "acme", "b-g")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.UpdateBudgetForTenant(ctx, "acme", &tables.TableBudget{ID: "b-a", MaxLimit: 150, ResetDuration: "1d"}))
	after, err := store.GetBudgetForTenant(ctx, "acme", "b-a")
	require.NoError(t, err)
	assert.Equal(t, float64(150), after.MaxLimit)

	err = store.DeleteBudgetForTenant(ctx, "acme", "b-g")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetBudgetForTenant(ctx, "globex", "b-g")
	require.NoError(t, err)

	require.NoError(t, store.DeleteBudgetForTenant(ctx, "acme", "b-a"))
	_, err = store.GetBudgetForTenant(ctx, "acme", "b-a")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestRateLimitsCRUDForTenant covers the full happy path on the
// tenant-scoped rate limit CRUD with cross-tenant isolation.
func TestRateLimitsCRUDForTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenLimit1 := int64(1000)
	tokenLimit2 := int64(2000)
	require.NoError(t, store.CreateRateLimitForTenant(ctx, "acme", &tables.TableRateLimit{
		ID: "rl-a", TokenMaxLimit: &tokenLimit1, TokenResetDuration: stringPtr("1m"),
	}))
	require.NoError(t, store.CreateRateLimitForTenant(ctx, "globex", &tables.TableRateLimit{
		ID: "rl-g", TokenMaxLimit: &tokenLimit2, TokenResetDuration: stringPtr("1m"),
	}))

	acme, err := store.GetRateLimitsByTenant(ctx, "acme")
	require.NoError(t, err)
	assert.Len(t, acme, 1)

	_, err = store.GetRateLimitForTenant(ctx, "acme", "rl-g")
	assert.ErrorIs(t, err, ErrNotFound)

	newLimit := int64(1500)
	require.NoError(t, store.UpdateRateLimitForTenant(ctx, "acme", &tables.TableRateLimit{
		ID: "rl-a", TokenMaxLimit: &newLimit, TokenResetDuration: stringPtr("1m"),
	}))
	after, err := store.GetRateLimitForTenant(ctx, "acme", "rl-a")
	require.NoError(t, err)
	require.NotNil(t, after.TokenMaxLimit)
	assert.Equal(t, int64(1500), *after.TokenMaxLimit)

	err = store.DeleteRateLimitForTenant(ctx, "acme", "rl-g")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetRateLimitForTenant(ctx, "globex", "rl-g")
	require.NoError(t, err)

	require.NoError(t, store.DeleteRateLimitForTenant(ctx, "acme", "rl-a"))
	_, err = store.GetRateLimitForTenant(ctx, "acme", "rl-a")
	assert.ErrorIs(t, err, ErrNotFound)
}

// stringPtr is a tiny helper for the rate-limit test above.
func stringPtr(s string) *string { return &s }

// TestCustomersCRUDForTenant covers the full happy path on the
// tenant-scoped customer CRUD: create + list-by-tenant returns own
// rows, update flips a field, delete removes from the tenant's view
// but cross-tenant delete probes are refused with ErrNotFound.
func TestCustomersCRUDForTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateCustomerForTenant(ctx, "acme", &tables.TableCustomer{ID: "c-acme", Name: "Acme Inc"}))
	require.NoError(t, store.CreateCustomerForTenant(ctx, "globex", &tables.TableCustomer{ID: "c-globex", Name: "Globex Inc"}))

	acme, err := store.GetCustomersByTenant(ctx, "acme")
	require.NoError(t, err)
	assert.Len(t, acme, 1)
	assert.Equal(t, "Acme Inc", acme[0].Name)

	globex, err := store.GetCustomersByTenant(ctx, "globex")
	require.NoError(t, err)
	assert.Len(t, globex, 1)

	_, err = store.GetCustomerByIDForTenant(ctx, "acme", "c-globex")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.UpdateCustomerForTenant(ctx, "acme", &tables.TableCustomer{ID: "c-acme", Name: "Acme LLC"}))
	after, err := store.GetCustomerByIDForTenant(ctx, "acme", "c-acme")
	require.NoError(t, err)
	assert.Equal(t, "Acme LLC", after.Name)

	err = store.DeleteCustomerForTenant(ctx, "acme", "c-globex")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetCustomerByIDForTenant(ctx, "globex", "c-globex")
	require.NoError(t, err)

	require.NoError(t, store.DeleteCustomerForTenant(ctx, "acme", "c-acme"))
	_, err = store.GetCustomerByIDForTenant(ctx, "acme", "c-acme")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestUpdateCustomerForTenant_RepinsTenantOnPayload defends against
// a malicious payload that tries to migrate a customer into another
// tenant via PUT.
func TestUpdateCustomerForTenant_RepinsTenantOnPayload(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateCustomerForTenant(ctx, "acme", &tables.TableCustomer{ID: "c-1", Name: "Acme"}))

	bad := &tables.TableCustomer{ID: "c-1", Name: "Acme-Renamed", TenantID: "globex"}
	require.NoError(t, store.UpdateCustomerForTenant(ctx, "acme", bad))

	_, err := store.GetCustomerByIDForTenant(ctx, "globex", "c-1")
	assert.ErrorIs(t, err, ErrNotFound)
	stillAcme, err := store.GetCustomerByIDForTenant(ctx, "acme", "c-1")
	require.NoError(t, err)
	assert.Equal(t, "acme", stillAcme.TenantID)
}

// TestTeamsCRUDForTenant covers the full happy path on the
// tenant-scoped team CRUD: create + list-by-tenant returns own rows,
// update flips a field, delete removes from the tenant's view but
// cross-tenant delete probes are refused with ErrNotFound.
func TestTeamsCRUDForTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create a team in each tenant.
	require.NoError(t, store.CreateTeamForTenant(ctx, "acme", &tables.TableTeam{ID: "t-acme", Name: "acme-eng"}))
	require.NoError(t, store.CreateTeamForTenant(ctx, "globex", &tables.TableTeam{ID: "t-globex", Name: "globex-eng"}))

	// List per tenant is isolated.
	acme, err := store.GetTeamsByTenant(ctx, "acme", "")
	require.NoError(t, err)
	assert.Len(t, acme, 1)
	assert.Equal(t, "acme-eng", acme[0].Name)

	globex, err := store.GetTeamsByTenant(ctx, "globex", "")
	require.NoError(t, err)
	assert.Len(t, globex, 1)

	// Cross-tenant GET-single returns ErrNotFound (not the foreign row).
	_, err = store.GetTeamByIDForTenant(ctx, "acme", "t-globex")
	assert.ErrorIs(t, err, ErrNotFound)

	// Update flips a field.
	updated := &tables.TableTeam{ID: "t-acme", Name: "acme-platform"}
	require.NoError(t, store.UpdateTeamForTenant(ctx, "acme", updated))
	after, err := store.GetTeamByIDForTenant(ctx, "acme", "t-acme")
	require.NoError(t, err)
	assert.Equal(t, "acme-platform", after.Name)

	// Cross-tenant DELETE returns ErrNotFound and globex's team survives.
	err = store.DeleteTeamForTenant(ctx, "acme", "t-globex")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetTeamByIDForTenant(ctx, "globex", "t-globex")
	require.NoError(t, err)

	// Own-tenant DELETE removes the row from its tenant's view.
	require.NoError(t, store.DeleteTeamForTenant(ctx, "acme", "t-acme"))
	_, err = store.GetTeamByIDForTenant(ctx, "acme", "t-acme")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestUpdateTeamForTenant_RepinsTenantOnPayload defends against a
// malicious payload that tries to migrate a team into a different
// tenant via PUT.
func TestUpdateTeamForTenant_RepinsTenantOnPayload(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateTeamForTenant(ctx, "acme", &tables.TableTeam{ID: "t-1", Name: "engineering"}))

	bad := &tables.TableTeam{ID: "t-1", Name: "engineering-renamed", TenantID: "globex"}
	require.NoError(t, store.UpdateTeamForTenant(ctx, "acme", bad))

	// The row stays with acme. globex still cannot see it.
	_, err := store.GetTeamByIDForTenant(ctx, "globex", "t-1")
	assert.ErrorIs(t, err, ErrNotFound)
	stillAcme, err := store.GetTeamByIDForTenant(ctx, "acme", "t-1")
	require.NoError(t, err)
	assert.Equal(t, "acme", stillAcme.TenantID)
}

// TestUpdateProviderForTenant_OwnTenantSucceeds covers the happy path —
// updating a tenant's provider config flips the requested field and
// is observable through the tenant-scoped read API.
func TestUpdateProviderForTenant_OwnTenantSucceeds(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	concurrency1, buf1 := 4, 16
	original := ProviderConfig{
		Keys:                     []schemas.Key{{ID: "acme-k1", Name: "acme-primary", Value: *schemas.NewEnvVar("sk-acme-1")}},
		ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: concurrency1, BufferSize: buf1},
	}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", original))

	concurrency2, buf2 := 8, 32
	updated := ProviderConfig{
		Keys:                     []schemas.Key{{ID: "acme-k1", Name: "acme-primary", Value: *schemas.NewEnvVar("sk-acme-1")}},
		ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: concurrency2, BufferSize: buf2},
	}
	require.NoError(t, store.UpdateProviderForTenant(ctx, "acme", "openai", updated))

	got, err := store.GetProviderConfigByTenant(ctx, "acme", "openai")
	require.NoError(t, err)
	require.NotNil(t, got.ConcurrencyAndBufferSize)
	assert.Equal(t, concurrency2, got.ConcurrencyAndBufferSize.Concurrency)
	assert.Equal(t, buf2, got.ConcurrencyAndBufferSize.BufferSize)
}

// TestUpdateProviderForTenant_OnlyOwnTenant proves a tenant-scoped
// UpdateProvider cannot trample another tenant's row even when the
// two share a provider name. The lookup is filtered by tenant_id, so
// acme's update operates on acme's openai; globex's openai keeps its
// original concurrency.
func TestUpdateProviderForTenant_OnlyOwnTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	concurrency1, buf1 := 4, 16
	concurrency2, buf2 := 8, 32
	make := func(c, b int, keyID string) ProviderConfig {
		return ProviderConfig{
			Keys:                     []schemas.Key{{ID: keyID, Name: keyID, Value: *schemas.NewEnvVar(keyID)}},
			ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: c, BufferSize: b},
		}
	}
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", make(concurrency1, buf1, "acme-k")))
	require.NoError(t, store.AddProviderForTenant(ctx, "globex", "openai", make(concurrency1, buf1, "globex-k")))

	// Acme bumps concurrency. Globex's row should not move.
	require.NoError(t, store.UpdateProviderForTenant(ctx, "acme", "openai", make(concurrency2, buf2, "acme-k")))

	acme, err := store.GetProviderConfigByTenant(ctx, "acme", "openai")
	require.NoError(t, err)
	assert.Equal(t, concurrency2, acme.ConcurrencyAndBufferSize.Concurrency)

	globex, err := store.GetProviderConfigByTenant(ctx, "globex", "openai")
	require.NoError(t, err)
	assert.Equal(t, concurrency1, globex.ConcurrencyAndBufferSize.Concurrency,
		"globex's openai must not have moved under acme's update")
}

// TestUpdateProviderForTenant_NewKey_PinsTenantID exercises the
// keys-cascade branch: adding a key via UpdateProvider's diff machinery
// should pin tenant_id on the new TableKey row so it stays visible
// only to the calling tenant.
func TestUpdateProviderForTenant_NewKey_PinsTenantID(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	concurrency, buf := 4, 16
	require.NoError(t, store.AddProviderForTenant(ctx, "acme", "openai", ProviderConfig{
		Keys:                     []schemas.Key{{ID: "acme-k1", Name: "acme-primary", Value: *schemas.NewEnvVar("sk-acme-1")}},
		ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: concurrency, BufferSize: buf},
	}))

	// Add a second key via update.
	require.NoError(t, store.UpdateProviderForTenant(ctx, "acme", "openai", ProviderConfig{
		Keys: []schemas.Key{
			{ID: "acme-k1", Name: "acme-primary", Value: *schemas.NewEnvVar("sk-acme-1")},
			{ID: "acme-k2", Name: "acme-secondary", Value: *schemas.NewEnvVar("sk-acme-2")},
		},
		ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: concurrency, BufferSize: buf},
	}))

	// Both keys must be visible to acme.
	acmeKeys, err := store.GetProviderKeysForTenant(ctx, "acme", "openai")
	require.NoError(t, err)
	assert.Len(t, acmeKeys, 2)

	// The new key's tenant_id is acme — not the column-default "default"
	// — so a default-tenant read would NOT see it.
	def, err := store.GetProvidersConfigByTenant(ctx, tables.DefaultTenantID)
	require.NoError(t, err)
	assert.Empty(t, def, "default tenant must not see acme's keys created via UpdateProvider")
}

// TestUpdateProviderForTenant_CrossTenantNotFound asserts that even
// when a provider with the requested name exists in some other
// tenant's scope, an update from the wrong tenant returns ErrNotFound.
func TestUpdateProviderForTenant_CrossTenantNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	concurrency, buf := 4, 16
	require.NoError(t, store.AddProviderForTenant(ctx, "globex", "openai", ProviderConfig{
		Keys:                     []schemas.Key{{ID: "g-k", Name: "g-k", Value: *schemas.NewEnvVar("sk-g")}},
		ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: concurrency, BufferSize: buf},
	}))

	err := store.UpdateProviderForTenant(ctx, "acme", "openai", ProviderConfig{
		ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: 99, BufferSize: 99},
	})
	assert.ErrorIs(t, err, ErrNotFound)

	// Globex's row stays at the original concurrency.
	globex, err := store.GetProviderConfigByTenant(ctx, "globex", "openai")
	require.NoError(t, err)
	assert.Equal(t, concurrency, globex.ConcurrencyAndBufferSize.Concurrency)
}

// TestUpdateVirtualKeyForTenant_OwnTenantSucceeds confirms a tenant
// can update its own VK and observe the change.
func TestUpdateVirtualKeyForTenant_OwnTenantSucceeds(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	isActive := true
	original := &tables.TableVirtualKey{
		ID: "vk-1", Name: "alpha", Value: "sk-bf-alpha", TenantID: "acme", IsActive: &isActive, Description: "before",
	}
	require.NoError(t, store.CreateVirtualKey(ctx, original))

	updated := &tables.TableVirtualKey{
		ID: "vk-1", Name: "alpha", Value: "sk-bf-alpha", TenantID: "acme", IsActive: &isActive, Description: "after",
	}
	require.NoError(t, store.UpdateVirtualKeyForTenant(ctx, "acme", updated))

	got, err := store.GetVirtualKeyByIDForTenant(ctx, "acme", "vk-1")
	require.NoError(t, err)
	assert.Equal(t, "after", got.Description, "update should be visible to acme")
}

// TestUpdateVirtualKeyForTenant_CrossTenantReturnsNotFound proves an
// admin in tenant A cannot mutate a VK that belongs to tenant B even
// when they correctly guess the id.
func TestUpdateVirtualKeyForTenant_CrossTenantReturnsNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	isActive := true
	require.NoError(t, store.CreateVirtualKey(ctx, &tables.TableVirtualKey{
		ID: "vk-secret", Name: "secret", Value: "sk-bf-secret", TenantID: "globex", IsActive: &isActive,
	}))

	err := store.UpdateVirtualKeyForTenant(ctx, "acme", &tables.TableVirtualKey{
		ID: "vk-secret", Name: "secret", Value: "sk-bf-secret", IsActive: &isActive, Description: "hijack attempt",
	})
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestUpdateVirtualKeyForTenant_RepinsTenantOnPayload defends against
// a malicious payload that tries to move the VK into a different
// tenant by setting TenantID in the body.
func TestUpdateVirtualKeyForTenant_RepinsTenantOnPayload(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	isActive := true
	require.NoError(t, store.CreateVirtualKey(ctx, &tables.TableVirtualKey{
		ID: "vk-1", Name: "alpha", Value: "sk-bf-alpha", TenantID: "acme", IsActive: &isActive,
	}))

	// Payload claims to be moving the VK into globex; should be re-
	// pinned to acme by UpdateVirtualKeyForTenant.
	bad := &tables.TableVirtualKey{
		ID: "vk-1", Name: "alpha", Value: "sk-bf-alpha", TenantID: "globex", IsActive: &isActive, Description: "moved?",
	}
	require.NoError(t, store.UpdateVirtualKeyForTenant(ctx, "acme", bad))

	// VK stays with acme. globex still cannot see it.
	_, err := store.GetVirtualKeyByIDForTenant(ctx, "globex", "vk-1")
	assert.ErrorIs(t, err, ErrNotFound)
	stillAcme, err := store.GetVirtualKeyByIDForTenant(ctx, "acme", "vk-1")
	require.NoError(t, err)
	assert.Equal(t, "acme", stillAcme.TenantID)
}

// TestUpdateMCPClientConfigForTenant_OwnTenantSucceeds confirms a
// tenant-scoped update flips a field and is observable in the
// tenant's read path.
func TestUpdateMCPClientConfigForTenant_OwnTenantSucceeds(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateMCPClientConfigForTenant(ctx, "acme", &schemas.MCPClientConfig{
		ID:               "mcp-1",
		Name:             "tools",
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewEnvVar("https://acme/mcp"),
	}))

	existing, err := store.GetMCPClientByIDForTenant(ctx, "acme", "mcp-1")
	require.NoError(t, err)
	existing.Disabled = true
	require.NoError(t, store.UpdateMCPClientConfigForTenant(ctx, "acme", "mcp-1", existing))

	after, err := store.GetMCPClientByIDForTenant(ctx, "acme", "mcp-1")
	require.NoError(t, err)
	assert.True(t, after.Disabled, "update should be visible to acme")
}

// TestUpdateMCPClientConfigForTenant_CrossTenantReturnsNotFound asserts
// the MCP equivalent of the VK cross-tenant isolation check.
func TestUpdateMCPClientConfigForTenant_CrossTenantReturnsNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateMCPClientConfigForTenant(ctx, "globex", &schemas.MCPClientConfig{
		ID:               "mcp-globex",
		Name:             "tools",
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewEnvVar("https://globex/mcp"),
	}))
	existing, err := store.GetMCPClientByIDForTenant(ctx, "globex", "mcp-globex")
	require.NoError(t, err)
	existing.Disabled = true

	err = store.UpdateMCPClientConfigForTenant(ctx, "acme", "mcp-globex", existing)
	assert.ErrorIs(t, err, ErrNotFound)

	// globex's row stays untouched.
	still, err := store.GetMCPClientByIDForTenant(ctx, "globex", "mcp-globex")
	require.NoError(t, err)
	assert.False(t, still.Disabled, "globex's row should not have flipped under a cross-tenant attempt")
}

// TestGetMCPClientByIDForTenant_CrossTenantReturnsNotFound covers the
// MCP equivalent of the VK isolation check.
func TestGetMCPClientByIDForTenant_CrossTenantReturnsNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateMCPClientConfigForTenant(ctx, "acme", &schemas.MCPClientConfig{
		ID:               "mcp-acme",
		Name:             "tools",
		ConnectionType:   schemas.MCPConnectionTypeHTTP,
		ConnectionString: schemas.NewEnvVar("https://acme/mcp"),
	}))

	got, err := store.GetMCPClientByIDForTenant(ctx, "acme", "mcp-acme")
	require.NoError(t, err)
	require.NotNil(t, got)

	_, err = store.GetMCPClientByIDForTenant(ctx, "globex", "mcp-acme")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestDeleteMCPClientConfigForTenant_OnlyOwnTenant covers the MCP
// equivalent of the VK delete-isolation check.
func TestDeleteMCPClientConfigForTenant_OnlyOwnTenant(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	for _, p := range []struct {
		tid, id string
	}{
		{"acme", "mcp-acme"},
		{"globex", "mcp-globex"},
	} {
		require.NoError(t, store.CreateMCPClientConfigForTenant(ctx, p.tid, &schemas.MCPClientConfig{
			ID:               p.id,
			Name:             "tools",
			ConnectionType:   schemas.MCPConnectionTypeHTTP,
			ConnectionString: schemas.NewEnvVar("https://" + p.tid + "/mcp"),
		}))
	}

	// Cross-tenant attempt fails, target survives.
	err := store.DeleteMCPClientConfigForTenant(ctx, "acme", "mcp-globex")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetMCPClientByIDForTenant(ctx, "globex", "mcp-globex")
	require.NoError(t, err)

	// Same-tenant delete works.
	require.NoError(t, store.DeleteMCPClientConfigForTenant(ctx, "acme", "mcp-acme"))
	_, err = store.GetMCPClientByIDForTenant(ctx, "acme", "mcp-acme")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestCreateMCPClientConfigForTenant_TwoTenantsSameName confirms (tenant_id,
// name) composite uniqueness lets two tenants both register an MCP client
// called "tools" without colliding.
func TestCreateMCPClientConfigForTenant_TwoTenantsSameName(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	makeMCP := func(id string, url string) *schemas.MCPClientConfig {
		return &schemas.MCPClientConfig{
			ID:               id,
			Name:             "tools",
			ConnectionType:   schemas.MCPConnectionTypeHTTP,
			ConnectionString: schemas.NewEnvVar(url),
		}
	}
	require.NoError(t, store.CreateMCPClientConfigForTenant(ctx, "acme", makeMCP("acme-mcp", "https://acme/mcp")))
	require.NoError(t, store.CreateMCPClientConfigForTenant(ctx, "globex", makeMCP("globex-mcp", "https://globex/mcp")))

	acme, err := store.GetMCPConfigByTenant(ctx, "acme")
	require.NoError(t, err)
	require.Len(t, acme.ClientConfigs, 1)
	assert.Equal(t, "acme-mcp", acme.ClientConfigs[0].ID)

	globex, err := store.GetMCPConfigByTenant(ctx, "globex")
	require.NoError(t, err)
	require.Len(t, globex.ClientConfigs, 1)
	assert.Equal(t, "globex-mcp", globex.ClientConfigs[0].ID)
}
