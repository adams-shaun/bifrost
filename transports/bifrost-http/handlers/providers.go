// Package handlers provides HTTP request handlers for the Bifrost HTTP transport.
// This file contains all provider management functionality including CRUD operations.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/modelcatalog"
	"github.com/maximhq/bifrost/multitenant"
	governanceplugin "github.com/maximhq/bifrost/plugins/governance"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// ModelsManager defines the interface for managing provider models
type ModelsManager interface {
	ReloadProvider(ctx context.Context, provider schemas.ModelProvider) (*tables.TableProvider, error)
	RemoveProvider(ctx context.Context, provider schemas.ModelProvider) error
	GetModelsForProvider(provider schemas.ModelProvider) []string
	GetUnfilteredModelsForProvider(provider schemas.ModelProvider) []string
}

// ProviderHandler manages HTTP requests for provider operations
type ProviderHandler struct {
	dbStore       configstore.ConfigStore
	inMemoryStore *lib.Config
	client        *bifrost.Bifrost
	modelsManager ModelsManager
}

// NewProviderHandler creates a new provider handler instance
func NewProviderHandler(modelsManager ModelsManager, inMemoryStore *lib.Config, client *bifrost.Bifrost) *ProviderHandler {
	return &ProviderHandler{
		dbStore:       inMemoryStore.ConfigStore,
		inMemoryStore: inMemoryStore,
		client:        client,
		modelsManager: modelsManager,
	}
}

type ProviderStatus = string

const (
	ProviderStatusActive  ProviderStatus = "active"  // Provider is active and working
	ProviderStatusError   ProviderStatus = "error"   // Provider failed to initialize
	ProviderStatusDeleted ProviderStatus = "deleted" // Provider is deleted from the store
)

// ProviderResponse represents the response for provider operations
type ProviderResponse struct {
	Name                     schemas.ModelProvider            `json:"name"`
	NetworkConfig            schemas.NetworkConfig            `json:"network_config"`                   // Network-related settings
	ConcurrencyAndBufferSize schemas.ConcurrencyAndBufferSize `json:"concurrency_and_buffer_size"`      // Concurrency settings
	ProxyConfig              *schemas.ProxyConfig             `json:"proxy_config"`                     // Proxy configuration
	SendBackRawRequest       bool                             `json:"send_back_raw_request"`            // Include raw request in BifrostResponse
	SendBackRawResponse      bool                             `json:"send_back_raw_response"`           // Include raw response in BifrostResponse
	StoreRawRequestResponse  bool                             `json:"store_raw_request_response"`       // Capture raw request/response for internal logging only
	CustomProviderConfig     *schemas.CustomProviderConfig    `json:"custom_provider_config,omitempty"` // Custom provider configuration
	OpenAIConfig             *schemas.OpenAIConfig            `json:"openai_config,omitempty"`          // OpenAI-specific configuration
	ProviderStatus           ProviderStatus                   `json:"provider_status"`                  // Health/initialization status of the provider
	Status                   string                           `json:"status,omitempty"`                 // Operational status (e.g., list_models_failed)
	Description              string                           `json:"description,omitempty"`            // Error/status description
	ConfigHash               string                           `json:"config_hash,omitempty"`            // Hash of config.json version, used for change detection
}

// ListProvidersResponse represents the response for listing all providers
type ListProvidersResponse struct {
	Providers []ProviderResponse `json:"providers"`
	Total     int                `json:"total"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

type providerCreatePayload struct {
	Provider                 schemas.ModelProvider             `json:"provider"`
	NetworkConfig            *schemas.NetworkConfig            `json:"network_config,omitempty"`
	ConcurrencyAndBufferSize *schemas.ConcurrencyAndBufferSize `json:"concurrency_and_buffer_size,omitempty"`
	ProxyConfig              *schemas.ProxyConfig              `json:"proxy_config,omitempty"`
	SendBackRawRequest       *bool                             `json:"send_back_raw_request,omitempty"`
	SendBackRawResponse      *bool                             `json:"send_back_raw_response,omitempty"`
	StoreRawRequestResponse  *bool                             `json:"store_raw_request_response,omitempty"`
	CustomProviderConfig     *schemas.CustomProviderConfig     `json:"custom_provider_config,omitempty"`
	OpenAIConfig             *schemas.OpenAIConfig             `json:"openai_config,omitempty"` // OpenAI-specific configuration
}

type providerUpdatePayload struct {
	NetworkConfig            schemas.NetworkConfig            `json:"network_config"`
	ConcurrencyAndBufferSize schemas.ConcurrencyAndBufferSize `json:"concurrency_and_buffer_size"`
	ProxyConfig              *schemas.ProxyConfig             `json:"proxy_config,omitempty"`
	SendBackRawRequest       *bool                            `json:"send_back_raw_request,omitempty"`
	SendBackRawResponse      *bool                            `json:"send_back_raw_response,omitempty"`
	StoreRawRequestResponse  *bool                            `json:"store_raw_request_response,omitempty"`
	CustomProviderConfig     *schemas.CustomProviderConfig    `json:"custom_provider_config,omitempty"`
	OpenAIConfig             *schemas.OpenAIConfig            `json:"openai_config,omitempty"` // OpenAI-specific configuration
}

// RegisterRoutes registers all provider management routes
func (h *ProviderHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	// Provider CRUD operations
	r.GET("/api/providers", lib.ChainMiddlewares(h.listProviders, middlewares...))
	r.GET("/api/providers/{provider}", lib.ChainMiddlewares(h.getProvider, middlewares...))
	r.GET("/api/providers/{provider}/keys", lib.ChainMiddlewares(h.listProviderKeys, middlewares...))
	r.GET("/api/providers/{provider}/keys/{key_id}", lib.ChainMiddlewares(h.getProviderKey, middlewares...))
	r.POST("/api/providers", lib.ChainMiddlewares(h.addProvider, middlewares...))
	r.POST("/api/providers/{provider}/keys", lib.ChainMiddlewares(h.createProviderKey, middlewares...))
	r.PUT("/api/providers/{provider}", lib.ChainMiddlewares(h.updateProvider, middlewares...))
	r.PUT("/api/providers/{provider}/keys/{key_id}", lib.ChainMiddlewares(h.updateProviderKey, middlewares...))
	r.DELETE("/api/providers/{provider}", lib.ChainMiddlewares(h.deleteProvider, middlewares...))
	r.DELETE("/api/providers/{provider}/keys/{key_id}", lib.ChainMiddlewares(h.deleteProviderKey, middlewares...))
	r.GET("/api/keys", lib.ChainMiddlewares(h.listKeys, middlewares...))
	r.GET("/api/models", lib.ChainMiddlewares(h.listModels, middlewares...))
	r.GET("/api/models/details", lib.ChainMiddlewares(h.listModelDetails, middlewares...))
	r.GET("/api/models/parameters", lib.ChainMiddlewares(h.getModelParameters, middlewares...))
	r.GET("/api/models/base", lib.ChainMiddlewares(h.listBaseModels, middlewares...))
}

// listProviders handles GET /api/providers - List all providers
func (h *ProviderHandler) listProviders(ctx *fasthttp.RequestCtx) {
	// Fetching providers from database or in-memory store
	var providers map[schemas.ModelProvider]configstore.ProviderConfig
	if h.dbStore != nil {
		var err error
		providers, err = h.dbStore.GetProvidersConfig(ctx)
		if err != nil {
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get providers: %v", err))
			return
		}
	} else {
		h.inMemoryStore.Mu.RLock()
		providers = h.inMemoryStore.Providers
		h.inMemoryStore.Mu.RUnlock()
	}
	providersInClient, err := h.client.GetConfiguredProviders()
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get providers from client: %v", err))
		return
	}
	providerResponses := []ProviderResponse{}

	for providerName, provider := range providers {
		config := provider.Redacted()

		providerStatus := ProviderStatusError
		if slices.Contains(providersInClient, providerName) {
			providerStatus = ProviderStatusActive
		}
		providerResponses = append(providerResponses, h.getProviderResponseFromConfig(providerName, *config, providerStatus))
	}
	// Sort providers alphabetically
	sort.Slice(providerResponses, func(i, j int) bool {
		return providerResponses[i].Name < providerResponses[j].Name
	})
	response := ListProvidersResponse{
		Providers: providerResponses,
		Total:     len(providerResponses),
	}

	SendJSON(ctx, response)
}

// getProvider handles GET /api/providers/{provider} - Get specific provider
func (h *ProviderHandler) getProvider(ctx *fasthttp.RequestCtx) {
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}

	providersInClient, err := h.client.GetConfiguredProviders()
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get providers from client: %v", err))
		return
	}

	var config *configstore.ProviderConfig
	if h.dbStore != nil {
		config, err = h.dbStore.GetProviderConfig(ctx, provider)
		if err != nil {
			if errors.Is(err, configstore.ErrNotFound) {
				SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider not found: %v", err))
				return
			}
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider config: %v", err))
			return
		}
	} else {
		config, err = h.inMemoryStore.GetProviderConfigRaw(provider)
		if err != nil {
			if errors.Is(err, lib.ErrNotFound) {
				SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider not found: %v", err))
				return
			}
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider config: %v", err))
			return
		}
	}
	redactedConfig := config.Redacted()

	providerStatus := ProviderStatusError
	if slices.Contains(providersInClient, provider) {
		providerStatus = ProviderStatusActive
	}

	response := h.getProviderResponseFromConfig(provider, *redactedConfig, providerStatus)

	SendJSON(ctx, response)
}

// addProvider handles POST /api/providers - Add a new provider
// NOTE: This only gets called when a new custom provider is added
func (h *ProviderHandler) addProvider(ctx *fasthttp.RequestCtx) {
	// Per-tenant runtime caches a snapshot of providers + keys taken at
	// Acquire time. Any mutation invalidates that snapshot for this
	// tenant — evict so the next inference re-loads from the DB.
	// Fires on error paths too: an over-eager evict just costs one
	// reload on the next request, which is identical to startup behavior.
	defer EvictTenant(ctx)

	var payload providerCreatePayload
	if err := sonic.Unmarshal(ctx.PostBody(), &payload); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	// Validate provider
	if payload.Provider == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Missing provider")
		return
	}
	if payload.CustomProviderConfig != nil {
		// custom provider key should not be same as standard provider names
		if bifrost.IsStandardProvider(payload.Provider) {
			SendError(ctx, fasthttp.StatusBadRequest, "Custom provider cannot be same as a standard provider")
			return
		}
		if payload.CustomProviderConfig.BaseProviderType == "" {
			SendError(ctx, fasthttp.StatusBadRequest, "BaseProviderType is required when CustomProviderConfig is provided")
			return
		}
		// check if base provider is a supported base provider
		if !bifrost.IsSupportedBaseProvider(payload.CustomProviderConfig.BaseProviderType) {
			SendError(ctx, fasthttp.StatusBadRequest, "BaseProviderType must be a standard provider")
			return
		}
	}
	if payload.ConcurrencyAndBufferSize != nil {
		if payload.ConcurrencyAndBufferSize.Concurrency == 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "Concurrency must be greater than 0")
			return
		}
		if payload.ConcurrencyAndBufferSize.BufferSize == 0 {
			SendError(ctx, fasthttp.StatusBadRequest, "Buffer size must be greater than 0")
			return
		}
		if payload.ConcurrencyAndBufferSize.Concurrency > payload.ConcurrencyAndBufferSize.BufferSize {
			SendError(ctx, fasthttp.StatusBadRequest, "Concurrency must be less than or equal to buffer size")
			return
		}
	}
	// Validate retry backoff values if NetworkConfig is provided
	if payload.NetworkConfig != nil {
		if err := validateRetryBackoff(payload.NetworkConfig); err != nil {
			SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid retry backoff: %v", err))
			return
		}
	}
	// Tenant-scoped existence check: hit the DB via the tenant-scope
	// GORM callback (h.dbStore.GetProviderConfig WHERE tenant_id = ctx
	// tenant) instead of the global in-memory c.Providers map. The
	// legacy in-memory check rejected creation if ANY tenant had the
	// provider name configured — a multi-tenant correctness bug, since
	// the DB carries composite (tenant_id, name) uniqueness so each
	// tenant can independently register e.g. "anthropic".
	if h.dbStore != nil {
		if existing, err := h.dbStore.GetProviderConfig(ctx, payload.Provider); err != nil {
			if !errors.Is(err, configstore.ErrNotFound) {
				SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to check provider config: %v", err))
				return
			}
		} else if existing != nil {
			SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Provider %s already exists for this tenant", payload.Provider))
			return
		}
	}

	// Construct ProviderConfig from individual fields
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
	// Validate custom provider configuration before persisting
	if err := lib.ValidateCustomProvider(config, payload.Provider); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid custom provider config: %v", err))
		return
	}
	// DB write via the configstore (tenant-scoped via the GORM
	// callback). Bypasses lib.Config.AddProvider's global in-memory
	// existence check so a name already owned by another tenant in
	// the shared c.Providers map does not 409 us.
	if h.dbStore != nil {
		if err := h.dbStore.AddProvider(ctx, payload.Provider, config); err != nil {
			if errors.Is(err, configstore.ErrAlreadyExists) {
				SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Provider %s already exists for this tenant", payload.Provider))
				return
			}
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to add provider: %v", err))
			return
		}
	}
	// Update the shared in-memory map best-effort so the legacy
	// inference runtime can see the new provider for this process
	// lifetime. Stage 2 will replace this with a tenant-keyed map;
	// for now a clobber is acceptable since the DB is the source of
	// truth and the inference runtime is shared across tenants anyway.
	h.inMemoryStore.Mu.Lock()
	if h.inMemoryStore.Providers == nil {
		h.inMemoryStore.Providers = map[schemas.ModelProvider]configstore.ProviderConfig{}
	}
	h.inMemoryStore.Providers[payload.Provider] = config
	h.inMemoryStore.Mu.Unlock()
	logger.Info("Provider %s added successfully", payload.Provider)

	if err := h.reloadProviderAfterCreate(ctx, payload.Provider); err != nil {
		logger.Warn("Failed to reload provider %s after add: %v", payload.Provider, err)
		// Rollback through dbStore directly so the DELETE flows the
		// request ctx's tenant_id through the GORM tenant-scope callback
		// (WHERE tenant_id = ?). Using context.Background() here would
		// drop the predicate and risk wiping the same provider name
		// from a sibling tenant.
		var rollbackErr error
		if h.dbStore != nil {
			rollbackErr = h.dbStore.DeleteProvider(ctx, payload.Provider)
		}
		// Best-effort in-memory cleanup.
		h.inMemoryStore.Mu.Lock()
		delete(h.inMemoryStore.Providers, payload.Provider)
		h.inMemoryStore.Mu.Unlock()
		if rollbackErr != nil && !errors.Is(rollbackErr, configstore.ErrNotFound) {
			logger.Error("Failed to rollback provider %s after reload failure: %v", payload.Provider, rollbackErr)
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to initialize provider after add: %v (rollback failed: %v)", err, rollbackErr))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to initialize provider after add: %v", err))
		return
	}

	// Get redacted config for response. Tenant-aware: dbStore in
	// multi-tenant mode (so the response reflects what THIS tenant just
	// stored, not whatever the global in-memory map happens to hold for
	// the same provider name); in-memory in single-tenant. See
	// providers_tenant_safe.go.
	redactedConfig, err := h.loadProviderConfigRedacted(ctx, payload.Provider)
	if err != nil {
		logger.Warn("Failed to get redacted config for provider %s: %v", payload.Provider, err)
		// Fall back to the raw config (no keys)
		response := h.getProviderResponseFromConfig(payload.Provider, configstore.ProviderConfig{
			NetworkConfig:            config.NetworkConfig,
			ConcurrencyAndBufferSize: config.ConcurrencyAndBufferSize,
			ProxyConfig:              config.ProxyConfig,
			SendBackRawRequest:       config.SendBackRawRequest,
			SendBackRawResponse:      config.SendBackRawResponse,
			StoreRawRequestResponse:  config.StoreRawRequestResponse,
			CustomProviderConfig:     config.CustomProviderConfig,
			Status:                   config.Status,
			Description:              config.Description,
		}, ProviderStatusActive)
		SendJSON(ctx, response)
		return
	}

	response := h.getProviderResponseFromConfig(payload.Provider, *redactedConfig, ProviderStatusActive)

	SendJSON(ctx, response)
}

// updateProvider handles PUT /api/providers/{provider} - Update provider config
// NOTE: This endpoint expects ALL fields to be provided in the request body,
// including both edited and non-edited fields. Partial updates are not supported.
// The frontend should send the complete provider configuration.
// This flow upserts the config
func (h *ProviderHandler) updateProvider(ctx *fasthttp.RequestCtx) {
	defer EvictTenant(ctx) // see addProvider for rationale

	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		// If not found, then first we create and then update
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}

	var payload = struct {
		Keys []schemas.Key `json:"keys"` // API keys for the provider
		providerUpdatePayload
	}{}

	if err := sonic.Unmarshal(ctx.PostBody(), &payload); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	// Get the raw config to access actual values for merging with redacted request values.
	// Tenant-aware: dbStore in multi-tenant mode (the shared in-memory
	// map can hold a DIFFERENT tenant's config for the same provider
	// name, and merging the request payload onto that wrong base then
	// writing it back is the core of T3 / things-we-missed #7).
	oldConfigRaw, err := h.loadProviderConfigRaw(ctx, provider)
	if err != nil {
		if !errors.Is(err, lib.ErrNotFound) {
			logger.Warn("Failed to get old config for provider %s: %v", provider, err)
			SendError(ctx, fasthttp.StatusInternalServerError, err.Error())
			return
		}
	}

	if oldConfigRaw == nil {
		oldConfigRaw = &configstore.ProviderConfig{}
	}

	oldRedactedConfig, err := h.loadProviderConfigRedacted(ctx, provider)
	if err != nil {
		if !errors.Is(err, lib.ErrNotFound) {
			logger.Warn("Failed to get old redacted config for provider %s: %v", provider, err)
			SendError(ctx, fasthttp.StatusInternalServerError, err.Error())
			return
		}
	}

	if oldRedactedConfig == nil {
		oldRedactedConfig = &configstore.ProviderConfig{}
	}

	// Construct ProviderConfig from individual fields (keys are managed separately via /keys endpoints).
	//
	// CRITICAL: re-fetch Keys from the tenant-scoped dbStore rather than reusing
	// oldConfigRaw.Keys (which came from the global in-memory c.Providers map).
	// In multi-tenant deployments c.Providers is shared across tenants, so the
	// in-memory keys may belong to a DIFFERENT tenant than the caller. When
	// UpdateProvider downstream diffs the request's Keys against the tenant-
	// scoped DB lookup, it treats the wrong-tenant keys as new rows and tries
	// INSERT — which trips the GLOBAL idx_key_id unique constraint with
	// "a record with this key id already exists". Refetching scoped keys here
	// makes the diff a no-op (every key already matches DB by key_id) so the
	// update touches only provider metadata, which is what the admin UI wants.
	keys := oldConfigRaw.Keys
	if h.dbStore != nil {
		if dbKeys, err := h.dbStore.GetProviderKeys(ctx, provider); err == nil {
			keys = dbKeys
		}
	}
	config := configstore.ProviderConfig{
		Keys:                     keys,
		NetworkConfig:            oldConfigRaw.NetworkConfig,
		ConcurrencyAndBufferSize: oldConfigRaw.ConcurrencyAndBufferSize,
		ProxyConfig:              oldConfigRaw.ProxyConfig,
		CustomProviderConfig:     oldConfigRaw.CustomProviderConfig,
		OpenAIConfig:             oldConfigRaw.OpenAIConfig,
		StoreRawRequestResponse:  oldConfigRaw.StoreRawRequestResponse,
		Status:                   oldConfigRaw.Status,
		Description:              oldConfigRaw.Description,
	}

	if payload.ConcurrencyAndBufferSize.Concurrency == 0 {
		SendError(ctx, fasthttp.StatusBadRequest, "Concurrency must be greater than 0")
		return
	}
	if payload.ConcurrencyAndBufferSize.BufferSize == 0 {
		SendError(ctx, fasthttp.StatusBadRequest, "Buffer size must be greater than 0")
		return
	}

	if payload.ConcurrencyAndBufferSize.Concurrency > payload.ConcurrencyAndBufferSize.BufferSize {
		SendError(ctx, fasthttp.StatusBadRequest, "Concurrency must be less than or equal to buffer size")
		return
	}

	// Build a prospective config with the requested CustomProviderConfig (including nil)
	prospective := config
	prospective.CustomProviderConfig = payload.CustomProviderConfig
	if err := lib.ValidateCustomProviderUpdate(prospective, *oldConfigRaw, provider); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid custom provider config: %v", err))
		return
	}

	nc := payload.NetworkConfig

	// Validate retry backoff values
	if err := validateRetryBackoff(&nc); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid retry backoff: %v", err))
		return
	}

	config.ConcurrencyAndBufferSize = &payload.ConcurrencyAndBufferSize
	// Merge network config - restore ca_cert_pem if the redacted placeholder was sent back
	if oldConfigRaw.NetworkConfig != nil && oldRedactedConfig.NetworkConfig != nil && nc.CACertPEM != nil {
		if nc.CACertPEM.IsRedacted() && nc.CACertPEM.Equals(oldRedactedConfig.NetworkConfig.CACertPEM) {
			nc.CACertPEM = oldConfigRaw.NetworkConfig.CACertPEM
		}
	}
	config.NetworkConfig = &nc
	// Merge proxy config - preserve secrets if redacted values were sent back
	if payload.ProxyConfig != nil && oldConfigRaw.ProxyConfig != nil && oldRedactedConfig.ProxyConfig != nil {
		if payload.ProxyConfig.URL != nil && payload.ProxyConfig.URL.IsRedacted() && payload.ProxyConfig.URL.Equals(oldRedactedConfig.ProxyConfig.URL) {
			payload.ProxyConfig.URL = oldConfigRaw.ProxyConfig.URL
		}
		if payload.ProxyConfig.Username != nil && payload.ProxyConfig.Username.IsRedacted() && payload.ProxyConfig.Username.Equals(oldRedactedConfig.ProxyConfig.Username) {
			payload.ProxyConfig.Username = oldConfigRaw.ProxyConfig.Username
		}
		if payload.ProxyConfig.Password != nil && payload.ProxyConfig.Password.IsRedacted() && payload.ProxyConfig.Password.Equals(oldRedactedConfig.ProxyConfig.Password) {
			payload.ProxyConfig.Password = oldConfigRaw.ProxyConfig.Password
		}
		if payload.ProxyConfig.CACertPEM != nil && payload.ProxyConfig.CACertPEM.IsRedacted() && payload.ProxyConfig.CACertPEM.Equals(oldRedactedConfig.ProxyConfig.CACertPEM) {
			payload.ProxyConfig.CACertPEM = oldConfigRaw.ProxyConfig.CACertPEM
		}
	}

	config.ProxyConfig = payload.ProxyConfig
	config.CustomProviderConfig = payload.CustomProviderConfig
	config.OpenAIConfig = payload.OpenAIConfig
	if payload.SendBackRawRequest != nil {
		config.SendBackRawRequest = *payload.SendBackRawRequest
	}
	if payload.SendBackRawResponse != nil {
		config.SendBackRawResponse = *payload.SendBackRawResponse
	}
	if payload.StoreRawRequestResponse != nil {
		config.StoreRawRequestResponse = *payload.StoreRawRequestResponse
	}

	// Add provider to store if it doesn't exist (upsert behavior).
	// Existence check goes through the tenant-aware helper so a row
	// belonging to a DIFFERENT tenant under the same provider name
	// doesn't satisfy this check (which would skip the AddProvider
	// branch and leave the calling tenant with no row at all in
	// multi-tenant mode).
	if _, err := h.loadProviderConfigRaw(ctx, provider); err != nil {
		if !errors.Is(err, lib.ErrNotFound) {
			logger.Warn("Failed to get provider %s: %v", provider, err)
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider: %v", err))
			return
		}
		// Adding the provider to store via the tenant-aware writer (see
		// providers_tenant_safe.go::commitProviderAdd).
		if err := h.commitProviderAdd(ctx, provider, config); err != nil {
			// In an upsert flow, "already exists" is not fatal — the provider may have been
			// added concurrently or exist in the DB from a previous failed attempt.
			if !errors.Is(err, lib.ErrAlreadyExists) {
				logger.Warn("Failed to add provider %s: %v", provider, err)
				SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to add provider: %v", err))
				return
			}
			logger.Info("Provider %s already exists during upsert, proceeding with update", provider)
		}
	}

	// Update provider config (env vars will be processed by store).
	// Tenant-aware: in multi-tenant mode this writes direct through
	// dbStore inside a transaction and DOES NOT touch the global
	// in-memory map or re-enter the legacy key-sync path. The
	// per-tenant runtime is invalidated by patch7's defer EvictTenant
	// at the top of this handler, so the next inference for this
	// tenant re-loads the freshly-written config.
	if err := h.commitProviderUpdate(ctx, provider, config); err != nil {
		logger.Warn("Failed to update provider %s: %v", provider, err)
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update provider: %v", err))
		return
	}
	// Attempt model discovery (also triggers cluster broadcast via ReloadProvider).
	// For keyless providers, model discovery is skipped but we still need to
	// call ReloadProvider directly so the config change is broadcast to cluster peers.
	if payload.CustomProviderConfig != nil && payload.CustomProviderConfig.IsKeyLess {
		ctxWithTimeout, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, reloadErr := h.modelsManager.ReloadProvider(ctxWithTimeout, provider); reloadErr != nil {
			logger.Warn("ReloadProvider failed for keyless provider %s: %v", provider, reloadErr)
		}
	} else {
		err = h.attemptModelDiscovery(ctx, provider, payload.CustomProviderConfig)
		if err != nil {
			logger.Warn("Model discovery failed for provider %s: %v", provider, err)
		}
	}

	// Get redacted config for response (in-memory store is now updated by updateKeyStatus
	// on single-tenant; multi-tenant goes through dbStore via the tenant-aware helper).
	redactedConfig, err := h.loadProviderConfigRedacted(ctx, provider)
	if err != nil {
		logger.Warn("Failed to get redacted config for provider %s: %v", provider, err)
		// Fall back to sanitized config (no keys)
		response := h.getProviderResponseFromConfig(provider, configstore.ProviderConfig{
			NetworkConfig:            config.NetworkConfig,
			ConcurrencyAndBufferSize: config.ConcurrencyAndBufferSize,
			ProxyConfig:              config.ProxyConfig,
			SendBackRawRequest:       config.SendBackRawRequest,
			SendBackRawResponse:      config.SendBackRawResponse,
			StoreRawRequestResponse:  config.StoreRawRequestResponse,
			CustomProviderConfig:     config.CustomProviderConfig,
			Status:                   config.Status,
			Description:              config.Description,
		}, ProviderStatusActive)
		SendJSON(ctx, response)
		return
	}

	response := h.getProviderResponseFromConfig(provider, *redactedConfig, ProviderStatusActive)

	SendJSON(ctx, response)
}

// deleteProvider handles DELETE /api/providers/{provider} - Remove provider
func (h *ProviderHandler) deleteProvider(ctx *fasthttp.RequestCtx) {
	defer EvictTenant(ctx) // see addProvider for rationale

	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}

	// Check if provider exists (tenant-aware — see providers_tenant_safe.go).
	if _, err := h.loadProviderConfigRedacted(ctx, provider); err != nil && !errors.Is(err, lib.ErrNotFound) {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Failed to get provider: %v", err))
		return
	}

	if err := h.modelsManager.RemoveProvider(ctx, provider); err != nil {
		logger.Warn("Failed to delete models for provider %s: %v", provider, err)
	}

	response := ProviderResponse{
		Name: provider,
	}

	SendJSON(ctx, response)
}

// listKeys handles GET /api/keys - List all keys
func (h *ProviderHandler) listKeys(ctx *fasthttp.RequestCtx) {
	keys, err := h.inMemoryStore.GetAllKeys()
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get keys: %v", err))
		return
	}

	SendJSON(ctx, keys)
}

// ModelResponse represents a single model in the response
type ModelResponse struct {
	Name             string   `json:"name"`
	Provider         string   `json:"provider"`
	AccessibleByKeys []string `json:"accessible_by_keys,omitempty"`
}

// ListModelsResponse represents the response for listing models
type ListModelsResponse struct {
	Models []ModelResponse `json:"models"`
	Total  int             `json:"total"`
}

// ModelDetailsResponse represents a model with capability metadata.
type ModelDetailsResponse struct {
	Name             string                `json:"name"`
	Provider         string                `json:"provider"`
	ContextLength    *int                  `json:"context_length,omitempty"`
	MaxInputTokens   *int                  `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  *int                  `json:"max_output_tokens,omitempty"`
	Architecture     *schemas.Architecture `json:"architecture,omitempty"`
	AccessibleByKeys []string              `json:"accessible_by_keys,omitempty"`
}

// ListModelDetailsResponse represents the response for listing detailed models.
type ListModelDetailsResponse struct {
	Models []ModelDetailsResponse `json:"models"`
	Total  int                    `json:"total"`
}

type modelListQuery struct {
	Provider   schemas.ModelProvider
	Query      string
	KeyIDs     []string
	Limit      int
	Unfiltered bool
	// VK-based filtering: populated when a virtual key is found in request headers.
	// HasVKFilter=true restricts providers/models to those allowed by the VK.
	HasVKFilter       bool
	VKProviderConfigs []tables.TableVirtualKeyProviderConfig
}

type listedModel struct {
	Name             string
	Provider         schemas.ModelProvider
	AccessibleByKeys []string
}

// listModels handles GET /api/models - List models with filtering
// Query parameters:
//   - query: Filter models by name (case-insensitive partial match)
//   - provider: Filter by specific provider name
//   - keys: Comma-separated list of provider key UUIDs to filter models accessible by those keys
//   - limit: Maximum number of results to return (default: 5)
//
// Request headers:
//   - x-bf-vk / Authorization: Bearer / x-api-key / x-goog-api-key: Virtual key (sk-bf-…) to scope
//     results to providers and models allowed by that virtual key.
func (h *ProviderHandler) listModels(ctx *fasthttp.RequestCtx) {
	query, ok := h.parseModelListQuery(ctx, 5)
	if !ok {
		return
	}
	allModels, total, err := h.listManagementModels(query)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get providers: %v", err))
		return
	}

	responseModels := make([]ModelResponse, 0, len(allModels))
	for _, model := range allModels {
		entry := ModelResponse{
			Name:     model.Name,
			Provider: string(model.Provider),
		}
		if len(model.AccessibleByKeys) > 0 {
			entry.AccessibleByKeys = model.AccessibleByKeys
		}
		responseModels = append(responseModels, entry)
	}

	response := ListModelsResponse{
		Models: responseModels,
		Total:  total,
	}

	SendJSON(ctx, response)
}

// listModelDetails handles GET /api/models/details - List models with capability metadata.
// Query parameters:
//   - query: Filter models by name (case-insensitive partial match)
//   - provider: Filter by specific provider name
//   - keys: Comma-separated list of key IDs to filter models accessible by those keys
//   - unfiltered: If true, bypass provider-level model pool restrictions only
//   - limit: Maximum number of results to return (default: 20)
//
// Request headers:
//   - x-bf-vk / Authorization: Bearer / x-api-key / x-goog-api-key: Virtual key (sk-bf-…) to scope
//     results to providers and models allowed by that virtual key.
func (h *ProviderHandler) listModelDetails(ctx *fasthttp.RequestCtx) {
	query, ok := h.parseModelListQuery(ctx, 20)
	if !ok {
		return
	}

	modelCatalog := h.inMemoryStore.ModelCatalog
	if modelCatalog == nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "model catalog not available")
		return
	}

	allModels, total, err := h.listManagementModels(query)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get providers: %v", err))
		return
	}

	responseModels := make([]ModelDetailsResponse, 0, len(allModels))
	for _, model := range allModels {
		details := ModelDetailsResponse{
			Name:     model.Name,
			Provider: string(model.Provider),
		}
		if len(model.AccessibleByKeys) > 0 {
			details.AccessibleByKeys = model.AccessibleByKeys
		}
		if capabilities := modelCatalog.GetModelCapabilityEntryForModel(model.Name, model.Provider); capabilities != nil {
			details.ContextLength = capabilities.ContextLength
			details.MaxInputTokens = capabilities.MaxInputTokens
			details.MaxOutputTokens = capabilities.MaxOutputTokens
			details.Architecture = capabilities.Architecture
		}
		responseModels = append(responseModels, details)
	}

	SendJSON(ctx, ListModelDetailsResponse{
		Models: responseModels,
		Total:  total,
	})
}

// parseModelListQuery normalizes the management model-list query string and resolves
// any virtual key present in the request headers to populate provider/model filters.
func (h *ProviderHandler) parseModelListQuery(ctx *fasthttp.RequestCtx, defaultLimit int) (modelListQuery, bool) {
	queryArgs := ctx.QueryArgs()
	query := modelListQuery{
		Provider:   schemas.ModelProvider(string(queryArgs.Peek("provider"))),
		Query:      string(queryArgs.Peek("query")),
		Limit:      defaultLimit,
		Unfiltered: string(queryArgs.Peek("unfiltered")) == "true",
	}

	if keysRaw := queryArgs.Peek("keys"); len(keysRaw) > 0 {
		keyIDs := strings.Split(string(keysRaw), ",")
		query.KeyIDs = make([]string, 0, len(keyIDs))
		for _, keyID := range keyIDs {
			trimmedKeyID := strings.TrimSpace(keyID)
			if trimmedKeyID == "" {
				continue
			}
			query.KeyIDs = append(query.KeyIDs, trimmedKeyID)
		}
	}

	if len(queryArgs.Peek("limit")) > 0 {
		if limit, err := queryArgs.GetUint("limit"); err == nil {
			query.Limit = limit
		}
	}

	// Resolve virtual key from request headers and populate provider/model filters.
	if vkValue := governanceplugin.ParseVirtualKeyFromFastHTTPRequest(ctx); vkValue != nil {
		trimmedVKValue := strings.TrimSpace(*vkValue)

		if h.dbStore == nil {
			SendError(ctx, fasthttp.StatusServiceUnavailable, "database store unavailable")
			return query, false
		}

		vk, err := h.dbStore.GetVirtualKeyByValue(ctx, trimmedVKValue)
		if err != nil {
			if !errors.Is(err, configstore.ErrNotFound) {
				SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to resolve virtual key: %v", err))
				return query, false
			}
		}

		if vk != nil {
			query.HasVKFilter = true
			query.VKProviderConfigs = vk.ProviderConfigs
		}
	}

	return query, true
}

// listManagementModels lists models across one or all providers and applies the top-level limit.
func (h *ProviderHandler) listManagementModels(query modelListQuery) ([]listedModel, int, error) {
	providers := []schemas.ModelProvider{}
	if query.Provider != "" {
		providers = append(providers, query.Provider)
	} else {
		var err error
		providers, err = h.inMemoryStore.GetAllProviders()
		if err != nil {
			return nil, 0, err
		}
	}

	// When a virtual key is present, restrict the provider list to those explicitly
	// permitted by the VK. An empty ProviderConfigs means no providers are allowed
	// (deny-by-default), so we return nothing even if providers are configured.
	if query.HasVKFilter {
		providers = slices.DeleteFunc(providers, func(p schemas.ModelProvider) bool {
			return !slices.ContainsFunc(query.VKProviderConfigs, func(pc tables.TableVirtualKeyProviderConfig) bool {
				return strings.EqualFold(pc.Provider, string(p))
			})
		})
	}

	models := make([]listedModel, 0)
	for _, provider := range providers {
		models = append(models, h.listManagementModelsForProvider(provider, query)...)
	}

	total := len(models)
	if query.Limit > 0 && query.Limit < len(models) {
		models = models[:query.Limit]
	}

	return models, total, nil
}

// listManagementModelsForProvider applies provider-level model selection and key filtering.
func (h *ProviderHandler) listManagementModelsForProvider(
	provider schemas.ModelProvider,
	query modelListQuery,
) []listedModel {
	models := h.modelsManager.GetModelsForProvider(provider)
	if query.Unfiltered {
		models = h.modelsManager.GetUnfilteredModelsForProvider(provider)
	}

	// Apply VK-level model whitelist filtering.
	// AllowedModels=["*"] passes all; empty AllowedModels denies all (deny-by-default).
	if query.HasVKFilter {
		if idx := slices.IndexFunc(query.VKProviderConfigs, func(pc tables.TableVirtualKeyProviderConfig) bool {
			return strings.EqualFold(pc.Provider, string(provider))
		}); idx >= 0 {
			allowedModels := query.VKProviderConfigs[idx].AllowedModels
			models = slices.DeleteFunc(models, func(m string) bool { return !allowedModels.IsAllowed(m) })
		}
	}

	if len(query.KeyIDs) == 0 || query.Unfiltered {
		return buildListedModels(provider, models, nil, query.Query)
	}

	// TODO(multitenant): this read is from the GLOBAL in-memory map and
	// can return another tenant's provider config when two tenants both
	// have providers named the same. Fixing requires threading ctx into
	// listManagementModelsForProvider — out of scope for T3 (which
	// targets the WRITE path); track separately as a future read-leak
	// follow-up alongside the line 203 dead-branch cleanup.
	config, err := h.inMemoryStore.GetProviderConfigRaw(provider)
	if err != nil {
		logger.Warn("Failed to get config for provider %s: %v", provider, err)
		return buildListedModels(provider, models, nil, query.Query)
	}
	if config == nil {
		logger.Warn("Failed to get config for provider %s: nil provider config", provider)
		return buildListedModels(provider, models, nil, query.Query)
	}

	validKeyIDs := getValidKeyIDsForProvider(config, query.KeyIDs)
	if len(validKeyIDs) == 0 {
		return buildListedModels(provider, models, nil, query.Query)
	}

	filteredModels, accessByModel := filterModelsByKeysWithAccessMap(
		config,
		provider,
		h.inMemoryStore.ModelCatalog,
		models,
		validKeyIDs,
	)

	return buildListedModels(provider, filteredModels, accessByModel, query.Query)
}

// buildListedModels filters model names by query and projects them into internal rows.
func buildListedModels(
	provider schemas.ModelProvider,
	models []string,
	accessByModel map[string][]string,
	query string,
) []listedModel {
	listedModels := make([]listedModel, 0, len(models))
	for _, model := range models {
		if !matchesModelQuery(model, query) {
			continue
		}

		entry := listedModel{
			Name:     model,
			Provider: provider,
		}
		if len(accessByModel[model]) > 0 {
			entry.AccessibleByKeys = accessByModel[model]
		}
		listedModels = append(listedModels, entry)
	}
	return listedModels
}

// getModelParameters handles GET /api/models/parameters - Get model parameters for a specific model
// Query parameters:
//   - model: The model name to get parameters for (required)
func (h *ProviderHandler) getModelParameters(ctx *fasthttp.RequestCtx) {
	modelParam := string(ctx.QueryArgs().Peek("model"))
	if modelParam == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "model query parameter is required")
		return
	}

	if h.dbStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "database store not available")
		return
	}

	params, err := h.dbStore.GetModelParametersByModel(ctx, modelParam)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("no parameters found for model %s", modelParam))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to get model parameters: %v", err))
		return
	}

	ctx.SetContentType("application/json")
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetBodyString(params.Data)
}

// keyAllowsModelForList reports whether a provider key permits model for catalog listing.
// When a non-nil catalog is provided, it also checks whether any allowlisted
// model resolves to the same base model name as the queried model (alias matching).
func keyAllowsModelForList(key schemas.Key, model string, catalog *modelcatalog.ModelCatalog) bool {
	if key.BlacklistedModels.IsBlocked(model) {
		return false
	}
	if len(key.Models) > 0 {
		if key.Models.IsAllowed(model) {
			return true
		}
		// Catalog-aware alias matching: a key allowlisting "gpt-4o-2024-08-06"
		// should also grant access to its base model "gpt-4o" in listings.
		if catalog != nil {
			for _, allowed := range key.Models {
				if strings.EqualFold(
					catalog.GetBaseModelName(allowed),
					catalog.GetBaseModelName(model),
				) {
					return true
				}
			}
		}
		return false
	}
	return true
}

// matchesModelQuery applies the shared query match used by /api/models,
// /api/models/details, and /api/models/base.
func matchesModelQuery(model, query string) bool {
	if query == "" {
		return true
	}

	queryLower := strings.ToLower(query)
	queryNormalized := strings.ReplaceAll(strings.ReplaceAll(queryLower, "-", ""), "_", "")
	modelLower := strings.ToLower(model)
	modelNormalized := strings.ReplaceAll(strings.ReplaceAll(modelLower, "-", ""), "_", "")

	return strings.Contains(modelLower, queryLower) ||
		strings.Contains(modelNormalized, queryNormalized) ||
		fuzzyMatch(modelLower, queryLower)
}

// getValidKeyIDsForProvider keeps only enabled, known, deduplicated key IDs.
func getValidKeyIDsForProvider(config *configstore.ProviderConfig, keyIDs []string) []string {
	if config == nil || len(keyIDs) == 0 {
		return nil
	}

	existing := make(map[string]bool, len(config.Keys))
	for _, key := range config.Keys {
		if key.Enabled != nil && !*key.Enabled {
			continue
		}
		existing[key.ID] = true
	}

	valid := make([]string, 0, len(keyIDs))
	seen := make(map[string]bool, len(keyIDs))
	for _, keyID := range keyIDs {
		if keyID == "" || seen[keyID] {
			continue
		}
		seen[keyID] = true
		if existing[keyID] {
			valid = append(valid, keyID)
		}
	}
	return valid
}

// filterModelsByKeysWithAccessMap filters models based on key-level model restrictions
// and returns the exact key IDs that grant access to each returned model.
func filterModelsByKeysWithAccessMap(config *configstore.ProviderConfig, provider schemas.ModelProvider, modelCatalog *modelcatalog.ModelCatalog, models []string, keyIDs []string) ([]string, map[string][]string) {
	if config == nil {
		return []string{}, map[string][]string{}
	}

	keysByID := make(map[string]schemas.Key, len(config.Keys))
	for _, key := range config.Keys {
		if key.Enabled != nil && !*key.Enabled {
			continue
		}
		keysByID[key.ID] = key
	}

	type matchedKey struct {
		id  string
		key schemas.Key
	}

	matchedKeys := make([]matchedKey, 0, len(keyIDs))
	for _, keyID := range keyIDs {
		key, ok := keysByID[keyID]
		if !ok {
			continue
		}
		matchedKeys = append(matchedKeys, matchedKey{id: keyID, key: key})
	}
	if len(matchedKeys) == 0 {
		return []string{}, map[string][]string{}
	}

	filtered := make([]string, 0, len(models))
	accessByModel := make(map[string][]string, len(models))
	for _, model := range models {
		grantedBy := make([]string, 0, len(matchedKeys))
		for _, matched := range matchedKeys {
			if keyAllowsModelForList(matched.key, model, modelCatalog) {
				grantedBy = append(grantedBy, matched.id)
			}
		}
		if len(grantedBy) == 0 {
			continue
		}
		filtered = append(filtered, model)
		accessByModel[model] = grantedBy
	}
	return filtered, accessByModel
}

// ListBaseModelsResponse represents the response for listing base models
type ListBaseModelsResponse struct {
	Models []string `json:"models"`
	Total  int      `json:"total"`
}

// listBaseModels handles GET /api/models/base - List distinct base model names from the catalog
// Query parameters:
//   - query: Filter base models by name (case-insensitive partial match)
//   - limit: Maximum number of results to return (default: 20)
func (h *ProviderHandler) listBaseModels(ctx *fasthttp.RequestCtx) {
	queryParam := string(ctx.QueryArgs().Peek("query"))
	limitParam := string(ctx.QueryArgs().Peek("limit"))

	limit := 20
	if limitParam != "" {
		if n, err := ctx.QueryArgs().GetUint("limit"); err == nil {
			limit = n
		}
	}

	modelCatalog := h.inMemoryStore.ModelCatalog
	if modelCatalog == nil {
		SendJSON(ctx, ListBaseModelsResponse{Models: []string{}, Total: 0})
		return
	}

	baseModels := modelCatalog.GetDistinctBaseModelNames()
	sort.Strings(baseModels)

	// Apply query filter if provided
	if queryParam != "" {
		filtered := []string{}
		for _, model := range baseModels {
			if matchesModelQuery(model, queryParam) {
				filtered = append(filtered, model)
			}
		}
		baseModels = filtered
	}

	total := len(baseModels)
	if limit > 0 && limit < len(baseModels) {
		baseModels = baseModels[:limit]
	}

	SendJSON(ctx, ListBaseModelsResponse{Models: baseModels, Total: total})
}

// reloadProviderAfterCreate performs a single bounded runtime reload after provider creation.
// ReloadProvider also refreshes model discovery, so create should not invoke a second discovery pass.
//
// f5xc-overlay (patch19): preserve the tenant id from the request ctx into the timeout
// context, same fix as attemptModelDiscovery — without it ReloadProvider can't extract
// the tid and BifrostFor falls back to the root runtime (defeating patch19's whole point).
// Also eager-evict before the reload so the tenant runtime gets rebuilt from a fresh DB
// snapshot that includes the just-POSTed provider.
func (h *ProviderHandler) reloadProviderAfterCreate(ctx *fasthttp.RequestCtx, provider schemas.ModelProvider) error {
	baseCtx := context.Background()
	if tid := TenantIDFromCtx(ctx); tid != "" {
		baseCtx = context.WithValue(baseCtx, multitenant.BifrostContextKeyTenantID, tid)
		EvictTenant(ctx)
	}
	ctxWithTimeout, cancel := context.WithTimeout(baseCtx, 15*time.Second)
	defer cancel()

	_, err := h.modelsManager.ReloadProvider(ctxWithTimeout, provider)
	return err
}

// attemptModelDiscovery performs model discovery with timeout
func (h *ProviderHandler) attemptModelDiscovery(ctx *fasthttp.RequestCtx, provider schemas.ModelProvider, customProviderConfig *schemas.CustomProviderConfig) error {
	// Determine if we should attempt model discovery
	shouldDiscoverModels := customProviderConfig == nil ||
		!customProviderConfig.IsKeyLess

	if !shouldDiscoverModels {
		return nil
	}

	// f5xc-overlay (patch18): preserve the tenant id from the request
	// ctx into the timeout context. Upstream used context.Background()
	// which drops the tenant, causing ReloadProvider's configstore
	// lookups to run unscoped (no GORM WHERE tenant_id = ?). Result:
	// for tenants whose provider name collides with another's, the
	// unscoped query returns rows from any tenant — typically wrong
	// keys, wrong network config. Stamping the tenant here keeps the
	// downstream GORM callbacks correctly scoped.
	baseCtx := context.Background()
	if tid := TenantIDFromCtx(ctx); tid != "" {
		baseCtx = context.WithValue(baseCtx, multitenant.BifrostContextKeyTenantID, tid)
		// f5xc-overlay (patch19): synchronously evict the tenant runtime
		// BEFORE discovery. ReloadProvider routes ListModelsRequest
		// through the tenant runtime (patch19) — but the runtime is
		// cached by multitenant.Manager, built lazily from a snapshot of
		// the configstore at first Acquire. If the caller is the key
		// create/update path (the common case for discovery), the just-
		// POSTed key is in the DB but NOT in the cached runtime's
		// StaticAccount. Calling EvictTenant here forces the next
		// BifrostFor inside ReloadProvider to rebuild the runtime from
		// a fresh DB read, so discovery sees the new key.
		//
		// The handler's `defer EvictTenant` still fires at function exit
		// to cover the inference path that runs AFTER this discovery
		// (so a follow-on inference also gets the freshest config); the
		// double-evict is harmless (Manager.Evict is a no-op when no
		// runtime is cached).
		EvictTenant(ctx)
	}
	ctxWithTimeout, cancel := context.WithTimeout(baseCtx, 15*time.Second)
	defer cancel()

	_, err := h.modelsManager.ReloadProvider(ctxWithTimeout, provider)
	if err != nil {
		return err
	}

	return nil
}

func (h *ProviderHandler) getProviderResponseFromConfig(provider schemas.ModelProvider, config configstore.ProviderConfig, status ProviderStatus) ProviderResponse {
	if config.NetworkConfig == nil {
		config.NetworkConfig = &schemas.DefaultNetworkConfig
	}
	if config.ConcurrencyAndBufferSize == nil {
		config.ConcurrencyAndBufferSize = &schemas.DefaultConcurrencyAndBufferSize
	}

	return ProviderResponse{
		Name:                     provider,
		NetworkConfig:            *config.NetworkConfig,
		ConcurrencyAndBufferSize: *config.ConcurrencyAndBufferSize,
		ProxyConfig:              config.ProxyConfig,
		SendBackRawRequest:       config.SendBackRawRequest,
		SendBackRawResponse:      config.SendBackRawResponse,
		StoreRawRequestResponse:  config.StoreRawRequestResponse,
		CustomProviderConfig:     config.CustomProviderConfig,
		OpenAIConfig:             config.OpenAIConfig,
		ProviderStatus:           status,
		Status:                   config.Status,
		Description:              config.Description,
		ConfigHash:               config.ConfigHash,
	}
}

func getProviderFromCtx(ctx *fasthttp.RequestCtx) (schemas.ModelProvider, error) {
	providerValue := ctx.UserValue("provider")
	if providerValue == nil {
		return "", fmt.Errorf("missing provider parameter")
	}
	providerStr, ok := providerValue.(string)
	if !ok {
		return "", fmt.Errorf("invalid provider parameter type")
	}

	decoded, err := url.PathUnescape(providerStr)
	if err != nil {
		return "", fmt.Errorf("invalid provider parameter encoding: %v", err)
	}

	return schemas.ModelProvider(decoded), nil
}

func validateRetryBackoff(networkConfig *schemas.NetworkConfig) error {
	if networkConfig != nil {
		if networkConfig.RetryBackoffInitial > 0 {
			if networkConfig.RetryBackoffInitial < lib.MinRetryBackoff {
				return fmt.Errorf("retry backoff initial must be at least %v", lib.MinRetryBackoff)
			}
			if networkConfig.RetryBackoffInitial > lib.MaxRetryBackoff {
				return fmt.Errorf("retry backoff initial must be at most %v", lib.MaxRetryBackoff)
			}
		}
		if networkConfig.RetryBackoffMax > 0 {
			if networkConfig.RetryBackoffMax < lib.MinRetryBackoff {
				return fmt.Errorf("retry backoff max must be at least %v", lib.MinRetryBackoff)
			}
			if networkConfig.RetryBackoffMax > lib.MaxRetryBackoff {
				return fmt.Errorf("retry backoff max must be at most %v", lib.MaxRetryBackoff)
			}
		}
		if networkConfig.RetryBackoffInitial > 0 && networkConfig.RetryBackoffMax > 0 {
			if networkConfig.RetryBackoffInitial > networkConfig.RetryBackoffMax {
				return fmt.Errorf("retry backoff initial must be less than or equal to retry backoff max")
			}
		}
	}
	return nil
}
