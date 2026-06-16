package multitenant

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"time"
)

// ErrUnknownVK is returned when a virtual key doesn't resolve to any tenant.
var ErrUnknownVK = errors.New("multitenant: unknown virtual key")

// VKResolver maps an inbound virtual key string to its owning TenantID.
//
// Production impl will be a Postgres lookup against governance_virtual_keys
// fronted by an LRU cache (and invalidated by LISTEN/NOTIFY). For the spike,
// MemoryVKResolver hardcodes the mapping.
type VKResolver interface {
	ResolveVK(ctx context.Context, vk string) (TenantID, error)
}

// VKResolverFunc adapts a function to VKResolver.
type VKResolverFunc func(ctx context.Context, vk string) (TenantID, error)

func (f VKResolverFunc) ResolveVK(ctx context.Context, vk string) (TenantID, error) {
	return f(ctx, vk)
}

// MemoryVKResolver is a fixed VK→TenantID map suitable for tests and the spike.
// Concurrent reads are safe; mutations are not.
type MemoryVKResolver struct {
	mapping map[string]TenantID
}

// NewMemoryVKResolver builds a resolver from a copy of m.
func NewMemoryVKResolver(m map[string]TenantID) *MemoryVKResolver {
	c := make(map[string]TenantID, len(m))
	for k, v := range m {
		c[k] = v
	}
	return &MemoryVKResolver{mapping: c}
}

func (r *MemoryVKResolver) ResolveVK(_ context.Context, vk string) (TenantID, error) {
	t, ok := r.mapping[vk]
	if !ok {
		return "", ErrUnknownVK
	}
	return t, nil
}

// CachedVKResolver wraps another resolver with an in-memory LRU-ish cache
// keyed by hash(vk). Entries expire after TTL. This mirrors the production
// design where Postgres backs the resolver but a TTL'd cache keeps the hot
// path off the DB.
//
// Spike-quality: not a true LRU; just TTL-eviction in a sync.Map. Fine for
// measuring resolver overhead.
type CachedVKResolver struct {
	backing VKResolver
	ttl     time.Duration

	mu    sync.RWMutex
	cache map[uint64]cachedVK
}

type cachedVK struct {
	tid    TenantID
	expiry time.Time
}

func NewCachedVKResolver(backing VKResolver, ttl time.Duration) *CachedVKResolver {
	return &CachedVKResolver{
		backing: backing,
		ttl:     ttl,
		cache:   make(map[uint64]cachedVK),
	}
}

func (r *CachedVKResolver) ResolveVK(ctx context.Context, vk string) (TenantID, error) {
	h := hashVK(vk)
	r.mu.RLock()
	if c, ok := r.cache[h]; ok && time.Now().Before(c.expiry) {
		r.mu.RUnlock()
		return c.tid, nil
	}
	r.mu.RUnlock()

	tid, err := r.backing.ResolveVK(ctx, vk)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.cache[h] = cachedVK{tid: tid, expiry: time.Now().Add(r.ttl)}
	r.mu.Unlock()
	return tid, nil
}

func hashVK(vk string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(vk))
	return h.Sum64()
}

// CacheSize returns the current number of cached VK entries. For telemetry/test.
func (r *CachedVKResolver) CacheSize() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// Invalidate drops a single VK from the cache. Same-process admin
// mutations (VK create/update/delete) call this so the resolver picks
// the new state up on the very next request rather than waiting for
// the TTL. Idempotent: invalidating an absent VK is a no-op.
//
// In multi-replica deployments the OTHER replicas' caches still TTL
// out the stale value within 60s; cross-replica invalidation is the
// "versioned-invalidation header / LISTEN/NOTIFY" follow-on.
func (r *CachedVKResolver) Invalidate(vk string) {
	if vk == "" {
		return
	}
	h := hashVK(vk)
	r.mu.Lock()
	delete(r.cache, h)
	r.mu.Unlock()
}

// InvalidateAll drops every cached entry. Useful when a downstream
// catastrophic event invalidates the entire VK<->tenant mapping (e.g.
// a DB restore) and finer-grained signals aren't available.
func (r *CachedVKResolver) InvalidateAll() {
	r.mu.Lock()
	for k := range r.cache {
		delete(r.cache, k)
	}
	r.mu.Unlock()
}
