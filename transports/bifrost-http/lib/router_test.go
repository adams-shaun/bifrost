package lib

import (
	"context"
	"sync"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
)

// fakeBifrost is a stand-in for *bifrost.Bifrost — we only need the pointer
// identity for SingleTenantRouter's tests; no methods are exercised.
func fakeBifrost(t *testing.T) *bifrost.Bifrost {
	t.Helper()
	return &bifrost.Bifrost{}
}

func TestSingleTenantRouter_AcquireReturnsSameClient(t *testing.T) {
	want := fakeBifrost(t)
	r := NewSingleTenantRouter(want)

	got, release, err := r.Acquire(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("Acquire returned %p, want %p", got, want)
	}
	if release == nil {
		t.Fatal("release must be non-nil")
	}
	// Release is a noop but must not panic.
	release()
}

func TestSingleTenantRouter_ConcurrentAcquireSafe(t *testing.T) {
	want := fakeBifrost(t)
	r := NewSingleTenantRouter(want)

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			got, release, err := r.Acquire(context.Background())
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			if got != want {
				t.Errorf("Acquire returned wrong client")
			}
			release()
		}()
	}
	wg.Wait()
}

func TestSingleTenantRouter_SetClient(t *testing.T) {
	first := fakeBifrost(t)
	r := NewSingleTenantRouter(first)
	if r.Client() != first {
		t.Fatal("Client should return the constructor argument")
	}

	second := fakeBifrost(t)
	r.SetClient(second)
	if r.Client() != second {
		t.Fatal("Client should return the most recent SetClient value")
	}
	got, release, _ := r.Acquire(context.Background())
	defer release()
	if got != second {
		t.Fatal("Acquire should serve the most recent SetClient value")
	}
}

func TestSingleTenantRouter_NilClientPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil client")
		}
	}()
	_ = NewSingleTenantRouter(nil)
}

func TestSingleTenantRouter_SetClientNilPanics(t *testing.T) {
	r := NewSingleTenantRouter(fakeBifrost(t))
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on SetClient(nil)")
		}
	}()
	r.SetClient(nil)
}
