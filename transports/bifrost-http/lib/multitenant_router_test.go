package lib

import (
	"context"
	"errors"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/multitenant"
)

// trivialLoader returns the cheapest possible BifrostConfig — used so
// the Manager can actually run bifrost.Init in tests without hitting a
// real provider.
func trivialLoader(_ context.Context, tid multitenant.TenantID) (schemas.BifrostConfig, error) {
	acct := multitenant.NewStaticAccount(tid)
	acct.AddProvider(schemas.OpenAI,
		[]schemas.Key{{ID: "k", Value: *schemas.NewEnvVar("fake"), Models: schemas.WhiteList{"*"}, Weight: 1.0}},
		&schemas.ProviderConfig{
			NetworkConfig:            schemas.DefaultNetworkConfig,
			ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 2, BufferSize: 4},
		},
	)
	return schemas.BifrostConfig{Account: acct, InitialPoolSize: 2}, nil
}

func newTestManager(t *testing.T) *multitenant.Manager {
	t.Helper()
	mgr, err := multitenant.NewManager(context.Background(), multitenant.ManagerConfig{
		Loader: trivialLoader,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	return mgr
}

func TestMultiTenantRouter_NilManagerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil Manager")
		}
	}()
	_ = NewMultiTenantRouter(MultiTenantRouterConfig{Manager: nil})
}

func TestMultiTenantRouter_AcquireUsesTenantOnCtx(t *testing.T) {
	mgr := newTestManager(t)
	r := NewMultiTenantRouter(MultiTenantRouterConfig{Manager: mgr})

	ctx := context.WithValue(context.Background(),
		multitenant.BifrostContextKeyTenantID, "acme")
	got, release, err := r.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got == nil {
		t.Fatal("expected *bifrost.Bifrost, got nil")
	}
	if release == nil {
		t.Fatal("release must be non-nil")
	}
	release()

	if mgr.ActiveRuntimes() != 1 {
		t.Fatalf("expected exactly one active runtime, got %d", mgr.ActiveRuntimes())
	}
}

func TestMultiTenantRouter_AcquireUsesStringFormKey(t *testing.T) {
	mgr := newTestManager(t)
	r := NewMultiTenantRouter(MultiTenantRouterConfig{Manager: mgr})

	// Resolver middleware writes the UserValue under the bare string
	// form of the key; the router falls back to that lookup so older
	// handler code paths still resolve.
	ctx := context.WithValue(context.Background(),
		string(multitenant.BifrostContextKeyTenantID), "acme")
	got, release, err := r.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got == nil {
		t.Fatal("expected *bifrost.Bifrost, got nil")
	}
	defer release()
}

func TestMultiTenantRouter_FallbackTenantWhenCtxBare(t *testing.T) {
	mgr := newTestManager(t)
	r := NewMultiTenantRouter(MultiTenantRouterConfig{
		Manager:        mgr,
		FallbackTenant: "default",
	})

	got, release, err := r.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	if got == nil {
		t.Fatal("expected fallback runtime, got nil")
	}
	if mgr.ActiveRuntimes() != 1 {
		t.Fatalf("fallback should have acquired the default tenant; active=%d", mgr.ActiveRuntimes())
	}
}

func TestMultiTenantRouter_FallbackClientWhenNoTenant(t *testing.T) {
	mgr := newTestManager(t)
	// Spare a fake legacy *bifrost.Bifrost — only the pointer identity matters
	// for the test; we never invoke methods on it.
	legacy := &bifrost.Bifrost{}
	r := NewMultiTenantRouter(MultiTenantRouterConfig{
		Manager:        mgr,
		FallbackClient: legacy,
	})

	got, release, err := r.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	if got != legacy {
		t.Fatal("expected the legacy fallback client")
	}
	if mgr.ActiveRuntimes() != 0 {
		t.Fatal("fallback client path must not touch the Manager")
	}
}

func TestMultiTenantRouter_NoTenantNoFallbackErrors(t *testing.T) {
	mgr := newTestManager(t)
	r := NewMultiTenantRouter(MultiTenantRouterConfig{Manager: mgr})

	_, _, err := r.Acquire(context.Background())
	if err == nil {
		t.Fatal("expected error when ctx has no tenant id and no fallback configured")
	}
}

func TestMultiTenantRouter_AcquireErrorPropagates(t *testing.T) {
	// Loader that always fails — exercises the error wrap path in
	// MultiTenantRouter.acquireTenant.
	failingLoader := func(_ context.Context, _ multitenant.TenantID) (schemas.BifrostConfig, error) {
		return schemas.BifrostConfig{}, errors.New("loader boom")
	}
	mgr, err := multitenant.NewManager(context.Background(), multitenant.ManagerConfig{Loader: failingLoader})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	r := NewMultiTenantRouter(MultiTenantRouterConfig{Manager: mgr})

	ctx := context.WithValue(context.Background(), multitenant.BifrostContextKeyTenantID, "acme")
	_, _, err = r.Acquire(ctx)
	if err == nil {
		t.Fatal("expected Acquire to return the loader's error")
	}
}

func TestMultiTenantRouter_ManagerAccessor(t *testing.T) {
	mgr := newTestManager(t)
	r := NewMultiTenantRouter(MultiTenantRouterConfig{Manager: mgr})
	if r.Manager() != mgr {
		t.Fatal("Manager accessor should return the constructor argument")
	}
}
