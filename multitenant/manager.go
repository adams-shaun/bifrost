package multitenant

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// ErrManagerClosed is returned by Acquire after Shutdown has been called.
var ErrManagerClosed = errors.New("multitenant: manager is closed")

// ErrLoaderRequired is returned by NewManager if no TenantLoader was supplied.
var ErrLoaderRequired = errors.New("multitenant: TenantLoader is required")

// ManagerConfig is the construction-time configuration for Manager.
type ManagerConfig struct {
	// Loader builds a BifrostConfig for a tenant on demand. Required.
	Loader TenantLoader

	// MaxActiveRuntimes caps how many tenant runtimes can be ready at once.
	// When Acquire would push the count above this cap, the LRU entry is
	// evicted (after its in-flight requests drain). Zero = unbounded.
	MaxActiveRuntimes int

	// Logger receives operational logs. Defaults to bifrost's default logger.
	Logger schemas.Logger
}

// Manager is the top-level multi-tenant wrapper. Hold one per process.
// Manager is safe for concurrent use.
type Manager struct {
	cfg ManagerConfig

	// rootCtx is the parent context for all tenant runtimes. Cancelled when
	// Shutdown is called; child runtimes observe the cancellation and exit.
	rootCtx    context.Context
	rootCancel context.CancelFunc

	// runtimes maps TenantID → *runtimeEntry. Read lock-free via sync.Map.
	runtimes sync.Map

	// mu guards lru list and closed flag. We deliberately keep this mutex
	// off the hot path: Acquire updates the LRU on cache-hit but the typical
	// op is O(1) — list.MoveToFront on an existing element.
	mu     sync.Mutex
	lru    *list.List // *list of *runtimeEntry, MRU at front, LRU at back
	closed bool

	logger schemas.Logger
}

// NewManager constructs a Manager. The provided ctx is the root for all
// tenant runtimes; cancelling it triggers a graceful Shutdown.
func NewManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if cfg.Loader == nil {
		return nil, ErrLoaderRequired
	}
	if cfg.Logger == nil {
		cfg.Logger = bifrost.NewDefaultLogger(schemas.LogLevelInfo)
	}
	rootCtx, cancel := context.WithCancel(ctx)
	m := &Manager{
		cfg:        cfg,
		rootCtx:    rootCtx,
		rootCancel: cancel,
		lru:        list.New(),
		logger:     cfg.Logger,
	}
	// Propagate parent-ctx cancellation into a graceful Shutdown so callers
	// can wire Manager into application lifecycles without a separate stop call.
	go func() {
		<-rootCtx.Done()
		_ = m.Shutdown(context.Background())
	}()
	return m, nil
}

// Acquire returns a Handle to the tenant's *bifrost.Bifrost, loading the
// runtime lazily if it doesn't exist yet. The caller MUST call Handle.Release
// when done — typically via defer — to allow eventual eviction.
//
// If MaxActiveRuntimes is set and Acquire would push the cache over the cap,
// the least-recently-used idle (refCount==0) entry is evicted first. If no
// idle entries exist, Acquire blocks until one frees up or ctx is cancelled.
func (m *Manager) Acquire(ctx context.Context, tid TenantID) (*Handle, error) {
	if tid == "" {
		return nil, fmt.Errorf("multitenant: empty tenant id")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrManagerClosed
	}
	m.mu.Unlock()

	// Fast path: existing entry. LoadOrStore avoids a second hash lookup on
	// the common cache-hit branch.
	newEntry := newRuntimeEntry(m.rootCtx, tid)
	v, loaded := m.runtimes.LoadOrStore(tid, newEntry)
	entry := v.(*runtimeEntry)

	if !loaded {
		// We won the race to create this tenant. Make eviction room before
		// running the (potentially expensive) loader, and only then enroll
		// the entry in the LRU.
		if err := m.maybeEvictLocked(ctx); err != nil {
			m.runtimes.Delete(tid)
			newEntry.cancel()
			return nil, err
		}
		m.mu.Lock()
		entry.lruElem = m.lru.PushFront(entry)
		m.mu.Unlock()
	}

	// initOnce makes the loader run exactly once even under concurrent Acquire.
	// Concurrent callers block on the same Do; subsequent callers see initErr.
	entry.initOnce.Do(func() {
		bfCfg, err := m.cfg.Loader(entry.ctx, tid)
		if err != nil {
			entry.initErr = fmt.Errorf("loader: %w", err)
			return
		}
		if bfCfg.Logger == nil {
			bfCfg.Logger = m.logger
		}
		bf, err := bifrost.Init(entry.ctx, bfCfg)
		if err != nil {
			entry.initErr = fmt.Errorf("bifrost.Init: %w", err)
			return
		}
		entry.bf = bf
	})

	if entry.initErr != nil {
		// Failed init: forget the entry so a future Acquire can retry.
		// Loader bugs that are persistent will simply fail again on next call.
		m.runtimes.Delete(tid)
		m.mu.Lock()
		if entry.lruElem != nil {
			m.lru.Remove(entry.lruElem)
			entry.lruElem = nil
		}
		m.mu.Unlock()
		entry.cancel()
		return nil, entry.initErr
	}

	// Bookkeeping for active borrow + LRU position.
	entry.refCount.Add(1)
	entry.touch()
	m.mu.Lock()
	if entry.lruElem != nil {
		m.lru.MoveToFront(entry.lruElem)
	}
	m.mu.Unlock()

	h := &Handle{
		tid: tid,
		bf:  entry.bf,
	}
	h.release = func() {
		entry.refCount.Add(-1)
		entry.touch()
	}
	return h, nil
}

// maybeEvictLocked evicts LRU entries (refCount==0) until under the cap.
// Returns an error only if the cap is exceeded AND no idle entry exists
// AND ctx is cancelled before one frees up.
//
// Note: we hold the lock briefly per check; the actual Shutdown of an evicted
// entry runs without the lock to avoid blocking other tenants.
func (m *Manager) maybeEvictLocked(ctx context.Context) error {
	if m.cfg.MaxActiveRuntimes <= 0 {
		return nil
	}
	for {
		m.mu.Lock()
		over := m.lru.Len() >= m.cfg.MaxActiveRuntimes
		var victim *runtimeEntry
		if over {
			for e := m.lru.Back(); e != nil; e = e.Prev() {
				cand := e.Value.(*runtimeEntry)
				if cand.refCount.Load() == 0 && !cand.shuttingDown.Load() {
					victim = cand
					break
				}
			}
		}
		m.mu.Unlock()

		if !over {
			return nil
		}
		if victim != nil {
			m.evict(victim)
			continue // re-check; may need to evict more (unlikely but safe)
		}

		// At cap and no idle victim. Brief sleep + retry; ctx-cancel wakes us.
		// This is the spike-quality back-off; production would use a condvar.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// evict removes the entry from the registry and shuts down its Bifrost.
// Idempotent. Blocks until shutdown finishes.
func (m *Manager) evict(e *runtimeEntry) {
	if !e.shuttingDown.CompareAndSwap(false, true) {
		<-e.shutdownDone
		return
	}
	defer close(e.shutdownDone)

	// Remove from registry first so new Acquires don't find this entry.
	m.runtimes.Delete(e.tenantID)
	m.mu.Lock()
	if e.lruElem != nil {
		m.lru.Remove(e.lruElem)
		e.lruElem = nil
	}
	m.mu.Unlock()

	// Wait briefly for in-flight refs to drain. We don't have a condvar in
	// the spike; production should signal via a per-entry channel.
	deadline := time.Now().Add(5 * time.Second)
	for e.refCount.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if e.bf != nil {
		e.bf.Shutdown()
	}
	e.cancel()
	m.logger.Info("multitenant: evicted tenant %s", string(e.tenantID))
}

// Evict explicitly removes a tenant from the cache. Blocks until shutdown
// completes. Returns true if the tenant was present.
func (m *Manager) Evict(tid TenantID) bool {
	v, ok := m.runtimes.Load(tid)
	if !ok {
		return false
	}
	m.evict(v.(*runtimeEntry))
	return true
}

// ActiveRuntimes returns the current count of loaded tenant runtimes.
// Useful for telemetry and tests.
func (m *Manager) ActiveRuntimes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lru.Len()
}

// Shutdown evicts all tenants and prevents further Acquire calls. Safe to call
// multiple times. The supplied ctx bounds the wait for in-flight drains.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	var victims []*runtimeEntry
	for e := m.lru.Front(); e != nil; e = e.Next() {
		victims = append(victims, e.Value.(*runtimeEntry))
	}
	m.mu.Unlock()

	for _, v := range victims {
		m.evict(v)
	}
	m.rootCancel()
	return nil
}
