package multitenant

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
)

// runtimeEntry holds one tenant's *bifrost.Bifrost plus lifecycle metadata.
// Loaded lazily by Manager.Acquire; evicted by LRU policy or explicit Evict.
//
// Lifecycle states:
//
//	uninitialized → loading → ready → shutting_down → shutdown
//
// `initOnce` ensures the loader runs exactly once even under concurrent Acquire
// calls. `refCount` tracks in-flight handles; eviction must wait for it to drop
// to zero before calling bifrost.Shutdown so producers don't race with channel
// teardown.
type runtimeEntry struct {
	tenantID TenantID

	initOnce sync.Once
	initErr  error
	bf       *bifrost.Bifrost
	ctx      context.Context // child of Manager.rootCtx; cancelled on evict
	cancel   context.CancelFunc

	refCount atomic.Int64 // in-flight Handle count; must be 0 to evict
	lastUsed atomic.Int64 // unix nano of last Acquire; updated lock-free

	// lru bookkeeping (guarded by Manager.mu)
	lruElem *list.Element

	// shutdown coordination
	shuttingDown atomic.Bool
	shutdownDone chan struct{}
}

func newRuntimeEntry(rootCtx context.Context, tid TenantID) *runtimeEntry {
	ctx, cancel := context.WithCancel(rootCtx)
	e := &runtimeEntry{
		tenantID:     tid,
		ctx:          ctx,
		cancel:       cancel,
		shutdownDone: make(chan struct{}),
	}
	e.lastUsed.Store(time.Now().UnixNano())
	return e
}

// touch updates the last-used timestamp. Cheap, lock-free.
func (e *runtimeEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

// Handle is a borrowed reference to a tenant's *bifrost.Bifrost.
// Callers MUST call Release() exactly once when done — typically via defer.
// Holding a Handle prevents the runtime from being evicted.
type Handle struct {
	tid     TenantID
	bf      *bifrost.Bifrost
	release func()
	once    sync.Once
}

// Bifrost returns the underlying *bifrost.Bifrost for the tenant. The pointer
// is valid only until Release() is called.
func (h *Handle) Bifrost() *bifrost.Bifrost { return h.bf }

// TenantID returns the tenant this handle was acquired for.
func (h *Handle) TenantID() TenantID { return h.tid }

// Release returns the handle to the Manager, decrementing the refcount and
// allowing eviction. Safe to call multiple times; only the first has effect.
func (h *Handle) Release() {
	h.once.Do(h.release)
}
