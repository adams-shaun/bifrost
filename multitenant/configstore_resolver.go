package multitenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/maximhq/bifrost/framework/configstore"
)

// vkLookup is the minimal slice of the configstore.ConfigStore API that
// ConfigStoreVKResolver needs. We accept this interface (rather than the
// whole ConfigStore) so unit tests can mock it without standing up a real
// SQLite-backed store.
//
// The single method matches configstore.ConfigStore.GetVirtualKeyByValue.
type vkLookup interface {
	GetVirtualKeyByValue(ctx context.Context, value string) (*virtualKeyMinimal, error)
}

// virtualKeyMinimal is the subset of tables.TableVirtualKey that the
// resolver reads. Defined separately so the interface contract stays small.
type virtualKeyMinimal struct {
	ID       string
	TenantID string
	IsActive *bool
}

// ConfigStoreVKResolver looks up a virtual key in framework/configstore's
// governance_virtual_keys table and returns its tenant_id. Wrap it in a
// CachedVKResolver to keep the hot path off the DB; the standard production
// pattern is:
//
//	memResolver := multitenant.NewConfigStoreVKResolver(store)
//	cached := multitenant.NewCachedVKResolver(memResolver, 60*time.Second)
//
// Returns ErrUnknownVK for keys that don't exist OR are marked inactive —
// these are treated identically so callers can't distinguish "wrong VK" from
// "disabled VK" via timing or error type.
type ConfigStoreVKResolver struct {
	store vkLookup
}

// NewConfigStoreVKResolver constructs a resolver backed by a ConfigStore.
// Passing nil panics — the resolver is useless without a backing store.
func NewConfigStoreVKResolver(store configstore.ConfigStore) *ConfigStoreVKResolver {
	if store == nil {
		panic("multitenant: nil ConfigStore passed to NewConfigStoreVKResolver")
	}
	return &ConfigStoreVKResolver{store: configStoreLookup{cs: store}}
}

// newConfigStoreVKResolverWithLookup is the test-only constructor that
// accepts a custom vkLookup so a mock can be substituted for the SQLite
// store. Kept unexported.
func newConfigStoreVKResolverWithLookup(lookup vkLookup) *ConfigStoreVKResolver {
	return &ConfigStoreVKResolver{store: lookup}
}

// ResolveVK implements VKResolver.
func (r *ConfigStoreVKResolver) ResolveVK(ctx context.Context, vk string) (TenantID, error) {
	if vk == "" {
		return "", ErrUnknownVK
	}
	row, err := r.store.GetVirtualKeyByValue(ctx, vk)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			return "", ErrUnknownVK
		}
		return "", fmt.Errorf("resolve VK: %w", err)
	}
	if row.IsActive != nil && !*row.IsActive {
		return "", ErrUnknownVK
	}
	if row.TenantID == "" {
		return "", fmt.Errorf("VK %q has no tenant_id (likely a pre-migration row that escaped the backfill)", row.ID)
	}
	return TenantID(row.TenantID), nil
}

// configStoreLookup adapts the full configstore.ConfigStore down to the
// narrower vkLookup interface, copying just the fields the resolver needs.
type configStoreLookup struct {
	cs configstore.ConfigStore
}

func (a configStoreLookup) GetVirtualKeyByValue(ctx context.Context, value string) (*virtualKeyMinimal, error) {
	row, err := a.cs.GetVirtualKeyByValue(ctx, value)
	if err != nil {
		return nil, err
	}
	return &virtualKeyMinimal{
		ID:       row.ID,
		TenantID: row.TenantID,
		IsActive: row.IsActive,
	}, nil
}
