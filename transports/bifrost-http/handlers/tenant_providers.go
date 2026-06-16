// Package handlers — tenant-scoped provider admin.
//
// This file mounts the /api/tenants/{tenant_id}/providers route family used
// by platform-admins to provision per-tenant provider configurations in
// multi-tenant deployments. The handlers read tenant_id from
// multitenant.BifrostContextKeyTenantID (populated by
// handlers.RequireTenantPathMiddleware) and call the *ForTenant variants
// on the ConfigStore so the row's tenant_id is pinned explicitly rather
// than relying on the column default.
//
// v1 ships POST only — the literal minimum for E2E to provision a per-tenant
// provider. GET/PUT/DELETE and provider keys follow in subsequent patches.
package handlers

import (
	"errors"
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/multitenant"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// TenantRuntimeEvictor is the narrow contract TenantProviderHandler needs
// from a multitenant.Manager so it can evict a tenant's cached Bifrost
// runtime after its config changes. Defined as an interface so tests can
// stub it without spinning up a real Manager.
type TenantRuntimeEvictor interface {
	Evict(tid multitenant.TenantID) bool
}

// TenantProviderHandler implements POST /api/tenants/{tenant_id}/providers
// — the minimum surface E2E needs to provision per-tenant providers.
type TenantProviderHandler struct {
	configStore configstore.ConfigStore
	evictor     TenantRuntimeEvictor
}

// NewTenantProviderHandler constructs a TenantProviderHandler.
//
// configStore is required; if nil, RegisterRoutes is a no-op so single-tenant
// deployments without a persistent store don't accidentally expose a broken
// route. evictor may be nil — when absent, mutations write to the store but
// do not bust the per-tenant runtime cache, so the next request for that
// tenant sees the new config only after natural LRU eviction.
func NewTenantProviderHandler(configStore configstore.ConfigStore, evictor TenantRuntimeEvictor) (*TenantProviderHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &TenantProviderHandler{configStore: configStore, evictor: evictor}, nil
}

// RegisterRoutes mounts the tenant-scoped provider admin routes. The
// supplied middlewares MUST include handlers.RequireTenantPathMiddleware
// (or an equivalent that lifts tenant_id onto the ctx) and a
// platform-admin authorization check.
func (h *TenantProviderHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	if h.configStore == nil {
		return
	}
	r.GET("/api/tenants/{tenant_id}/providers", lib.ChainMiddlewares(h.listProviders, middlewares...))
	r.POST("/api/tenants/{tenant_id}/providers", lib.ChainMiddlewares(h.createProvider, middlewares...))
	r.GET("/api/tenants/{tenant_id}/providers/{provider}", lib.ChainMiddlewares(h.getProvider, middlewares...))
	r.PUT("/api/tenants/{tenant_id}/providers/{provider}", lib.ChainMiddlewares(h.updateProvider, middlewares...))
	r.DELETE("/api/tenants/{tenant_id}/providers/{provider}", lib.ChainMiddlewares(h.deleteProvider, middlewares...))
	r.GET("/api/tenants/{tenant_id}/providers/{provider}/keys", lib.ChainMiddlewares(h.listProviderKeys, middlewares...))
	r.POST("/api/tenants/{tenant_id}/providers/{provider}/keys", lib.ChainMiddlewares(h.createProviderKey, middlewares...))
	r.GET("/api/tenants/{tenant_id}/providers/{provider}/keys/{key_id}", lib.ChainMiddlewares(h.getProviderKey, middlewares...))
	r.PUT("/api/tenants/{tenant_id}/providers/{provider}/keys/{key_id}", lib.ChainMiddlewares(h.updateProviderKey, middlewares...))
	r.DELETE("/api/tenants/{tenant_id}/providers/{provider}/keys/{key_id}", lib.ChainMiddlewares(h.deleteProviderKey, middlewares...))
}

// createProvider — POST /api/tenants/{tenant_id}/providers
//
// Validates the payload using the same rules as the legacy
// ProviderHandler.addProvider (custom provider checks, concurrency >= 1,
// retry backoff), then persists via ConfigStore.AddProviderForTenant with
// the tenant_id resolved from ctx. On success, evicts the tenant's cached
// Bifrost runtime so the next inference request lazy-loads with the new
// provider in scope.
func (h *TenantProviderHandler) createProvider(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}

	var payload providerCreatePayload
	if err := sonic.Unmarshal(ctx.PostBody(), &payload); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if payload.Provider == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Missing provider")
		return
	}
	if payload.CustomProviderConfig != nil {
		if bifrost.IsStandardProvider(payload.Provider) {
			SendError(ctx, fasthttp.StatusBadRequest, "Custom provider cannot be same as a standard provider")
			return
		}
		if payload.CustomProviderConfig.BaseProviderType == "" {
			SendError(ctx, fasthttp.StatusBadRequest, "BaseProviderType is required when CustomProviderConfig is provided")
			return
		}
		if !bifrost.IsSupportedBaseProvider(payload.CustomProviderConfig.BaseProviderType) {
			SendError(ctx, fasthttp.StatusBadRequest, "BaseProviderType must be a standard provider")
			return
		}
	}
	if payload.ConcurrencyAndBufferSize != nil {
		cb := payload.ConcurrencyAndBufferSize
		if cb.Concurrency == 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "Concurrency must be greater than 0")
			return
		}
		if cb.BufferSize == 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "Buffer size must be greater than 0")
			return
		}
		if cb.Concurrency > cb.BufferSize {
			SendError(ctx, fasthttp.StatusBadRequest, "Concurrency must be less than or equal to buffer size")
			return
		}
	}
	if payload.NetworkConfig != nil {
		if err := validateRetryBackoff(payload.NetworkConfig); err != nil {
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid retry backoff: %v", err))
			return
		}
	}

	config := configstore.ProviderConfig{
		NetworkConfig:            payload.NetworkConfig,
		ProxyConfig:              payload.ProxyConfig,
		ConcurrencyAndBufferSize: payload.ConcurrencyAndBufferSize,
		SendBackRawRequest:       payload.SendBackRawRequest != nil && *payload.SendBackRawRequest,
		SendBackRawResponse:      payload.SendBackRawResponse != nil && *payload.SendBackRawResponse,
		StoreRawRequestResponse:  payload.StoreRawRequestResponse != nil && *payload.StoreRawRequestResponse,
		CustomProviderConfig:     payload.CustomProviderConfig,
		OpenAIConfig:             payload.OpenAIConfig,
	}
	if err := lib.ValidateCustomProvider(config, payload.Provider); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid custom provider config: %v", err))
		return
	}

	if err := h.configStore.AddProviderForTenant(ctx, string(tid), payload.Provider, config); err != nil {
		if errors.Is(err, configstore.ErrAlreadyExists) {
			SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Provider %s already exists for tenant %s", payload.Provider, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to add provider: %v", err))
		return
	}

	// Evict the cached per-tenant Bifrost runtime so the next inference
	// request for this tenant lazy-loads with the new provider visible.
	// Best-effort: a missing evictor or a tenant with no live runtime are
	// not errors — they just mean there is nothing to invalidate.
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}

	// Return the same rich ProviderResponse shape the legacy POST
	// /api/providers returns so the UI's optimistic cache update
	// (which expects Name + NetworkConfig + ConcurrencyAndBufferSize
	// + status) round-trips correctly. A slimmer {tenant_id, provider}
	// shape would silently break the providers-list refresh under the
	// Stage 2 baseQuery rewriter, since the optimistic push would
	// drop a half-populated row into the cache.
	SendJSONWithStatus(ctx, buildTenantProviderResponse(payload.Provider, config), fasthttp.StatusCreated)
}

// buildTenantProviderResponse mirrors ProviderHandler.getProviderResponseFromConfig
// without taking a *ProviderHandler receiver, so the tenant-scoped
// handlers can return the same wire shape without depending on the
// legacy handler's lifecycle. tenant_id is added as an extra field —
// surfaced for clients that filter the response by tenant, harmless to
// the UI's typed ModelProvider (it just ignores unknown fields).
func buildTenantProviderResponse(provider schemas.ModelProvider, config configstore.ProviderConfig) map[string]any {
	if config.NetworkConfig == nil {
		nc := schemas.DefaultNetworkConfig
		config.NetworkConfig = &nc
	}
	if config.ConcurrencyAndBufferSize == nil {
		cb := schemas.DefaultConcurrencyAndBufferSize
		config.ConcurrencyAndBufferSize = &cb
	}
	return map[string]any{
		"name":                        provider,
		"network_config":              *config.NetworkConfig,
		"concurrency_and_buffer_size": *config.ConcurrencyAndBufferSize,
		"proxy_config":                config.ProxyConfig,
		"send_back_raw_request":       config.SendBackRawRequest,
		"send_back_raw_response":      config.SendBackRawResponse,
		"store_raw_request_response":  config.StoreRawRequestResponse,
		"custom_provider_config":      config.CustomProviderConfig,
		"openai_config":               config.OpenAIConfig,
		"provider_status":             ProviderStatusActive,
		"status":                      config.Status,
		"description":                 config.Description,
		"config_hash":                 config.ConfigHash,
	}
}

// listProviders — GET /api/tenants/{tenant_id}/providers
//
// Returns the tenant's providers in the SAME wire shape as the legacy
// /api/providers list endpoint (ListProvidersResponse: providers as
// an ARRAY of ProviderResponse, plus total). The UI's getProviders
// query keys off response.providers[].name; a map-keyed-by-name
// response would flatten to a list of empty ModelProvider objects
// and the providers list would render as zero rows.
func (h *TenantProviderHandler) listProviders(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providers, err := h.configStore.GetProvidersConfigByTenant(ctx, string(tid))
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to list providers: %v", err))
		return
	}
	rows := make([]map[string]any, 0, len(providers))
	for name, cfg := range providers {
		rows = append(rows, buildTenantProviderResponse(name, *cfg.Redacted()))
	}
	SendJSON(ctx, map[string]any{
		"providers": rows,
		"total":     len(rows),
	})
}

// getProvider — GET /api/tenants/{tenant_id}/providers/{provider}
func (h *TenantProviderHandler) getProvider(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providerStr, _ := ctx.UserValue("provider").(string)
	if providerStr == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "provider is required")
		return
	}
	cfg, err := h.configStore.GetProviderConfigByTenant(ctx, string(tid), schemas.ModelProvider(providerStr))
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider %s not found for tenant %s", providerStr, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider: %v", err))
		return
	}
	// Return the same rich ProviderResponse shape getProvider's UI
	// caller (and the optimistic cache update on PUT) expects — Name
	// at the top level, NetworkConfig + ConcurrencyAndBufferSize +
	// status fields alongside, NOT nested under a "config" key.
	SendJSON(ctx, buildTenantProviderResponse(schemas.ModelProvider(providerStr), *cfg.Redacted()))
}

// updateProvider — PUT /api/tenants/{tenant_id}/providers/{provider}
//
// Body uses the same providerUpdatePayload shape as the legacy
// /api/providers PUT (NetworkConfig + ConcurrencyAndBufferSize +
// ProxyConfig + raw-request/response flags + CustomProviderConfig +
// OpenAIConfig). Keys carry over from the existing provider via the
// in-memory merge below — operators rotate keys through the nested
// /keys routes rather than passing them in the provider payload.
//
// On success: per-tenant runtime is evicted so the next inference
// request acquires with the new config.
func (h *TenantProviderHandler) updateProvider(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providerStr, _ := ctx.UserValue("provider").(string)
	if providerStr == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "provider is required")
		return
	}

	var payload providerUpdatePayload
	if err := sonic.Unmarshal(ctx.PostBody(), &payload); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if payload.ConcurrencyAndBufferSize.Concurrency != 0 || payload.ConcurrencyAndBufferSize.BufferSize != 0 {
		cb := payload.ConcurrencyAndBufferSize
		if cb.Concurrency == 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "Concurrency must be greater than 0")
			return
		}
		if cb.BufferSize == 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "Buffer size must be greater than 0")
			return
		}
		if cb.Concurrency > cb.BufferSize {
			SendError(ctx, fasthttp.StatusBadRequest, "Concurrency must be less than or equal to buffer size")
			return
		}
	}
	if err := validateRetryBackoff(&payload.NetworkConfig); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid retry backoff: %v", err))
		return
	}

	// Load the existing config so we can carry keys forward (the legacy
	// /api/providers PUT shape doesn't ship keys; rotation flows go
	// through the nested /keys routes instead).
	existing, err := h.configStore.GetProviderConfigByTenant(ctx, string(tid), schemas.ModelProvider(providerStr))
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider %s not found for tenant %s", providerStr, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load provider: %v", err))
		return
	}

	updated := configstore.ProviderConfig{
		Keys:                     existing.Keys,
		NetworkConfig:            &payload.NetworkConfig,
		ConcurrencyAndBufferSize: &payload.ConcurrencyAndBufferSize,
		ProxyConfig:              payload.ProxyConfig,
		SendBackRawRequest:       payload.SendBackRawRequest != nil && *payload.SendBackRawRequest,
		SendBackRawResponse:      payload.SendBackRawResponse != nil && *payload.SendBackRawResponse,
		StoreRawRequestResponse:  payload.StoreRawRequestResponse != nil && *payload.StoreRawRequestResponse,
		CustomProviderConfig:     payload.CustomProviderConfig,
		OpenAIConfig:             payload.OpenAIConfig,
	}
	if err := lib.ValidateCustomProviderUpdate(updated, *existing, schemas.ModelProvider(providerStr)); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid custom provider update: %v", err))
		return
	}

	if err := h.configStore.UpdateProviderForTenant(ctx, string(tid), schemas.ModelProvider(providerStr), updated); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider %s not found for tenant %s", providerStr, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update provider: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	// Return rich ProviderResponse so the UI's updateProvider
	// mutation's onQueryStarted can update the getProviders cache
	// without crashing — sortProviders reads `name`, which a
	// {tenant_id, provider, config} shape wouldn't surface; the
	// undefined .name would throw inside .localeCompare and the
	// React error boundary would show "Something went wrong".
	SendJSON(ctx, buildTenantProviderResponse(schemas.ModelProvider(providerStr), *updated.Redacted()))
}

// deleteProvider — DELETE /api/tenants/{tenant_id}/providers/{provider}
//
// 404 when the provider doesn't exist in this tenant's scope, 204 on
// success. Evicts the per-tenant runtime so the next inference request
// lazy-loads with the provider removed.
func (h *TenantProviderHandler) deleteProvider(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providerStr, _ := ctx.UserValue("provider").(string)
	if providerStr == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "provider is required")
		return
	}
	if err := h.configStore.DeleteProviderForTenant(ctx, string(tid), schemas.ModelProvider(providerStr)); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider %s not found for tenant %s", providerStr, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete provider: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	ctx.SetStatusCode(fasthttp.StatusNoContent)
}

// providerKeyCreatePayload mirrors the legacy POST
// /api/providers/{provider}/keys body so the UI's add-key form (which
// sends the full schemas.Key — name, value, models, weight, aliases,
// blacklisted_models, etc.) round-trips through the tenant-scoped
// route without silently dropping fields.
//
// Embedding schemas.Key directly means we inherit every wire-format
// field schema in the project — no per-field shadowing.
type providerKeyCreatePayload struct {
	schemas.Key
}

// listProviderKeys — GET /api/tenants/{tenant_id}/providers/{provider}/keys
func (h *TenantProviderHandler) listProviderKeys(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providerStr, _ := ctx.UserValue("provider").(string)
	if providerStr == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "provider is required")
		return
	}
	keys, err := h.configStore.GetProviderKeysForTenant(ctx, string(tid), schemas.ModelProvider(providerStr))
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider %s not found for tenant %s", providerStr, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to list keys: %v", err))
		return
	}
	redactedKeys := make([]schemas.Key, len(keys))
	for i, k := range keys {
		redactedKeys[i] = k
		redactedKeys[i].Value = *schemas.NewEnvVar("REDACTED")
	}
	SendJSON(ctx, map[string]any{
		"tenant_id": string(tid),
		"provider":  providerStr,
		"keys":      redactedKeys,
		"total":     len(redactedKeys),
	})
}

// createProviderKey — POST /api/tenants/{tenant_id}/providers/{provider}/keys
//
// Adds a new key to a per-tenant provider. The new TableKey row's
// tenant_id is pinned to the calling tenant so cross-tenant reads
// don't pick it up. The per-tenant runtime is evicted so the next
// inference acquires the new key set.
func (h *TenantProviderHandler) createProviderKey(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providerStr, _ := ctx.UserValue("provider").(string)
	if providerStr == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "provider is required")
		return
	}

	var payload providerKeyCreatePayload
	if err := sonic.Unmarshal(ctx.PostBody(), &payload); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if payload.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "name is required")
		return
	}
	if payload.Value.GetValue() == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "value is required")
		return
	}
	if payload.ID == "" {
		payload.ID = uuid.NewString()
	}
	// Default Models to wildcard when the caller omitted it so a freshly
	// attached key can drive inference without a follow-up edit. The
	// legacy single-tenant flow had the same default; preserving it
	// keeps form behavior identical across tenant-scoped vs legacy.
	if len(payload.Models) == 0 {
		payload.Models = []string{"*"}
	}

	key := payload.Key
	if err := h.configStore.CreateProviderKeyForTenant(ctx, string(tid), schemas.ModelProvider(providerStr), key); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider %s not found for tenant %s", providerStr, tid))
			return
		}
		if errors.Is(err, configstore.ErrAlreadyExists) {
			SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Key already exists for tenant %s / provider %s", tid, providerStr))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create key: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	// Return the full schemas.Key shape so the UI's optimistic provider-
	// keys cache update has every field its TypeScript type expects
	// (models, weight, enabled, aliases, …). Redact the value on the
	// way out — secrets never leave the server in plaintext after the
	// create response, matching listProviderKeys's behavior.
	respKey := key
	respKey.Value = *schemas.NewEnvVar("REDACTED")
	SendJSONWithStatus(ctx, respKey, fasthttp.StatusCreated)
}

// getProviderKey — GET /api/tenants/{tenant_id}/providers/{provider}/keys/{key_id}
//
// Value is redacted on the wire; rotation flows should call create+delete
// rather than reading back the existing secret.
func (h *TenantProviderHandler) getProviderKey(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providerStr, _ := ctx.UserValue("provider").(string)
	keyID, _ := ctx.UserValue("key_id").(string)
	if providerStr == "" || keyID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "provider and key_id are required")
		return
	}
	key, err := h.configStore.GetProviderKeyForTenant(ctx, string(tid), schemas.ModelProvider(providerStr), keyID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Key %s not found for tenant %s / provider %s", keyID, tid, providerStr))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get key: %v", err))
		return
	}
	redacted := *key
	redacted.Value = *schemas.NewEnvVar("REDACTED")
	SendJSON(ctx, map[string]any{
		"tenant_id": string(tid),
		"provider":  providerStr,
		"key":       redacted,
	})
}

// updateProviderKey — PUT /api/tenants/{tenant_id}/providers/{provider}/keys/{key_id}
//
// Updates an existing key in place. Body accepts the full schemas.Key
// shape (name, value, models, weight, aliases, enabled, …) so the UI's
// edit-key form round-trips end-to-end. ID is read from the URL, NOT
// the body — the path is the source of truth so a typo'd id in the
// payload can't redirect the update to a different key.
//
// Cross-tenant safety: we GetProviderKeyForTenant first to confirm
// the key belongs to the calling tenant, then UpdateProviderKey
// (which works against the global key row). Without the tenant
// verification step a tenant could PUT another tenant's key by
// guessing the id; the GET check makes that a 404.
//
// Value rotation: passing a new value in the body rotates the key in
// place. Inference resumes against the new value on the next request
// (after the per-tenant runtime eviction below picks up the change).
func (h *TenantProviderHandler) updateProviderKey(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providerStr, _ := ctx.UserValue("provider").(string)
	keyID, _ := ctx.UserValue("key_id").(string)
	if providerStr == "" || keyID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "provider and key_id are required")
		return
	}
	// Verify the key belongs to this tenant before mutating.
	existing, err := h.configStore.GetProviderKeyForTenant(ctx, string(tid), schemas.ModelProvider(providerStr), keyID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Key %s not found for tenant %s / provider %s", keyID, tid, providerStr))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load key: %v", err))
		return
	}
	var payload schemas.Key
	if err := sonic.Unmarshal(ctx.PostBody(), &payload); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	// Pin id from the URL; the body's id is ignored. Carry forward
	// fields the body left blank so partial-update payloads don't
	// silently null out existing state.
	payload.ID = keyID
	if payload.Name == "" {
		payload.Name = existing.Name
	}
	if payload.Value.GetValue() == "" {
		// No new value supplied — keep the existing one. The persisted
		// EnvVar carries the encrypted blob; we don't need to decrypt
		// here because UpdateProviderKey writes the whole row.
		payload.Value = existing.Value
	}
	if len(payload.Models) == 0 {
		payload.Models = existing.Models
	}

	if err := h.configStore.UpdateProviderKey(ctx, schemas.ModelProvider(providerStr), keyID, payload); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Key %s not found for provider %s", keyID, providerStr))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update key: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	// Redact the value before responding — secrets only leave the
	// server on the create response; subsequent reads/updates redact.
	respKey := payload
	respKey.Value = *schemas.NewEnvVar("REDACTED")
	SendJSON(ctx, respKey)
}

// deleteProviderKey — DELETE /api/tenants/{tenant_id}/providers/{provider}/keys/{key_id}
func (h *TenantProviderHandler) deleteProviderKey(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	providerStr, _ := ctx.UserValue("provider").(string)
	keyID, _ := ctx.UserValue("key_id").(string)
	if providerStr == "" || keyID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "provider and key_id are required")
		return
	}
	if err := h.configStore.DeleteProviderKeyForTenant(ctx, string(tid), schemas.ModelProvider(providerStr), keyID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Key %s not found for tenant %s / provider %s", keyID, tid, providerStr))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete key: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	ctx.SetStatusCode(fasthttp.StatusNoContent)
}

// tenantIDFromCtx pulls the tenant_id off the fasthttp ctx, returning
// (tid, false) when missing. Centralised here so callers don't repeat the
// user-value lookup + type assertion.
func tenantIDFromCtx(ctx *fasthttp.RequestCtx) (multitenant.TenantID, bool) {
	v := ctx.UserValue(string(multitenant.BifrostContextKeyTenantID))
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return multitenant.TenantID(s), true
}
