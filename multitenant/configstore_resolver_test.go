package multitenant

import (
	"context"
	"errors"
	"testing"

	"github.com/maximhq/bifrost/framework/configstore"
)

// stubLookup is a vkLookup that returns canned rows / errors keyed by VK
// value. It lets us drive ConfigStoreVKResolver without standing up a real
// SQLite store.
type stubLookup struct {
	byValue map[string]*virtualKeyMinimal
	err     error
}

func (s *stubLookup) GetVirtualKeyByValue(_ context.Context, value string) (*virtualKeyMinimal, error) {
	if s.err != nil {
		return nil, s.err
	}
	row, ok := s.byValue[value]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func boolPtr(b bool) *bool { return &b }

func TestConfigStoreVKResolver_HappyPath(t *testing.T) {
	lookup := &stubLookup{byValue: map[string]*virtualKeyMinimal{
		"sk-bf-good": {ID: "vk-1", TenantID: "acme"},
	}}
	r := newConfigStoreVKResolverWithLookup(lookup)

	tid, err := r.ResolveVK(context.Background(), "sk-bf-good")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tid != "acme" {
		t.Fatalf("got tid=%q want %q", tid, "acme")
	}
}

func TestConfigStoreVKResolver_UnknownVK(t *testing.T) {
	r := newConfigStoreVKResolverWithLookup(&stubLookup{byValue: map[string]*virtualKeyMinimal{}})
	if _, err := r.ResolveVK(context.Background(), "sk-bf-missing"); !errors.Is(err, ErrUnknownVK) {
		t.Fatalf("expected ErrUnknownVK, got %v", err)
	}
}

func TestConfigStoreVKResolver_EmptyVK(t *testing.T) {
	r := newConfigStoreVKResolverWithLookup(&stubLookup{})
	if _, err := r.ResolveVK(context.Background(), ""); !errors.Is(err, ErrUnknownVK) {
		t.Fatalf("empty VK should return ErrUnknownVK, got %v", err)
	}
}

func TestConfigStoreVKResolver_InactiveVK(t *testing.T) {
	lookup := &stubLookup{byValue: map[string]*virtualKeyMinimal{
		"sk-bf-disabled": {ID: "vk-d", TenantID: "acme", IsActive: boolPtr(false)},
	}}
	r := newConfigStoreVKResolverWithLookup(lookup)

	// IsActive=false collapses into ErrUnknownVK so callers can't tell
	// "wrong VK" from "disabled VK" by error type.
	if _, err := r.ResolveVK(context.Background(), "sk-bf-disabled"); !errors.Is(err, ErrUnknownVK) {
		t.Fatalf("inactive VK should return ErrUnknownVK, got %v", err)
	}

	// IsActive=true is normal.
	lookup.byValue["sk-bf-on"] = &virtualKeyMinimal{ID: "vk-on", TenantID: "acme", IsActive: boolPtr(true)}
	tid, err := r.ResolveVK(context.Background(), "sk-bf-on")
	if err != nil || tid != "acme" {
		t.Fatalf("active VK with IsActive=true should resolve, got tid=%q err=%v", tid, err)
	}

	// IsActive=nil is treated as active (DB default behaviour).
	lookup.byValue["sk-bf-nil"] = &virtualKeyMinimal{ID: "vk-nil", TenantID: "acme"}
	tid, err = r.ResolveVK(context.Background(), "sk-bf-nil")
	if err != nil || tid != "acme" {
		t.Fatalf("IsActive=nil should be treated as active, got tid=%q err=%v", tid, err)
	}
}

func TestConfigStoreVKResolver_MissingTenantIDIsAnError(t *testing.T) {
	// VK exists but TenantID is empty — that's a corrupted row (pre-migration
	// data that escaped the backfill). Surface a real error so it gets noticed
	// rather than silently routing to "" tenant.
	lookup := &stubLookup{byValue: map[string]*virtualKeyMinimal{
		"sk-bf-orphan": {ID: "vk-orphan", TenantID: ""},
	}}
	r := newConfigStoreVKResolverWithLookup(lookup)

	_, err := r.ResolveVK(context.Background(), "sk-bf-orphan")
	if err == nil {
		t.Fatal("expected an error for missing tenant_id")
	}
	if errors.Is(err, ErrUnknownVK) {
		t.Fatalf("missing tenant_id should NOT collapse to ErrUnknownVK; got %v", err)
	}
}

func TestConfigStoreVKResolver_LookupErrorPropagates(t *testing.T) {
	wantErr := errors.New("db borked")
	r := newConfigStoreVKResolverWithLookup(&stubLookup{err: wantErr})

	_, err := r.ResolveVK(context.Background(), "sk-bf-anything")
	if err == nil {
		t.Fatal("expected backing error to propagate")
	}
	if errors.Is(err, ErrUnknownVK) {
		t.Fatalf("backing DB error should NOT collapse to ErrUnknownVK; got %v", err)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped DB error, got %v", err)
	}
}

func TestConfigStoreVKResolver_NilConfigStorePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil ConfigStore")
		}
	}()
	_ = NewConfigStoreVKResolver(nil)
}
