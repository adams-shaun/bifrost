package handlers

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// _ keeps lib imported even when no compile-time reference remains
// after a refactor; remove once a non-trivial lib usage returns.
var _ = lib.ErrNotFound

// ListProviderKeysResponse represents the response for listing keys for a provider.
type ListProviderKeysResponse struct {
	Keys  []schemas.Key `json:"keys"`
	Total int           `json:"total"`
}

func (h *ProviderHandler) listProviderKeys(ctx *fasthttp.RequestCtx) {
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}

	if h.dbStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "Config store not configured")
		return
	}
	// Tenant-scoped via GORM callback. Read provider config first so a
	// caller hitting an unknown-to-this-tenant provider gets 404 rather
	// than an empty key list.
	if _, err := h.dbStore.GetProviderConfig(ctx, provider); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider config: %v", err))
		return
	}
	rawKeys, err := h.dbStore.GetProviderKeys(ctx, provider)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider keys: %v", err))
		return
	}
	redactedKeys := make([]schemas.Key, len(rawKeys))
	for i, k := range rawKeys {
		redactedKeys[i] = k
		redactedKeys[i].Value = *k.Value.Redacted()
	}
	SendJSON(ctx, ListProviderKeysResponse{Keys: redactedKeys, Total: len(redactedKeys)})
}

func (h *ProviderHandler) getProviderKey(ctx *fasthttp.RequestCtx) {
	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}

	keyID, err := getKeyIDFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if h.dbStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "Config store not configured")
		return
	}
	rawKey, err := h.dbStore.GetProviderKey(ctx, provider, keyID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider key not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider key: %v", err))
		return
	}
	redacted := *rawKey
	redacted.Value = *rawKey.Value.Redacted()
	SendJSON(ctx, redacted)
}

func (h *ProviderHandler) createProviderKey(ctx *fasthttp.RequestCtx) {
	defer EvictTenant(ctx) // see providers.addProvider for rationale

	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}

	var key schemas.Key
	if err := sonic.Unmarshal(ctx.PostBody(), &key); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	if h.dbStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "Config store not configured")
		return
	}
	providerConfig, err := h.dbStore.GetProviderConfig(ctx, provider)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider config: %v", err))
		return
	}

	if providerConfig.CustomProviderConfig != nil && providerConfig.CustomProviderConfig.IsKeyLess {
		SendError(ctx, fasthttp.StatusBadRequest, "Cannot add keys to a keyless provider")
		return
	}

	baseProvider := provider
	if providerConfig.CustomProviderConfig != nil && providerConfig.CustomProviderConfig.BaseProviderType != "" {
		baseProvider = providerConfig.CustomProviderConfig.BaseProviderType
	}

	if !bifrost.CanProviderKeyValueBeEmpty(baseProvider) && key.Value.GetValue() == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Key value must not be empty")
		return
	}

	if err := validateProviderKeyURL(provider, key); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if err := key.BlacklistedModels.Validate(); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid blacklisted_models: %v", err))
		return
	}

	if err := key.Aliases.Validate(); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid aliases: %v", err))
		return
	}

	if key.ID == "" {
		key.ID = uuid.NewString()
	}
	if key.Enabled == nil {
		key.Enabled = bifrost.Ptr(true)
	}

	if err := h.dbStore.CreateProviderKey(ctx, provider, key); err != nil {
		logger.Warn("Failed to create key for provider %s: %v", provider, err)
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider not found: %v", err))
			return
		}
		if errors.Is(err, configstore.ErrAlreadyExists) {
			SendError(ctx, fasthttp.StatusConflict, err.Error())
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create provider key: %v", err))
		return
	}

	// Best-effort in-memory sync so the shared inference runtime can
	// route to the new key without a restart. Stage 2 replaces this
	// with a tenant-keyed map.
	h.inMemoryStore.Mu.Lock()
	if cfg, ok := h.inMemoryStore.Providers[provider]; ok {
		cfg.Keys = append(cfg.Keys, key)
		h.inMemoryStore.Providers[provider] = cfg
	}
	h.inMemoryStore.Mu.Unlock()

	if err := h.attemptModelDiscovery(ctx, provider, providerConfig.CustomProviderConfig); err != nil {
		logger.Warn("Model discovery failed for provider %s after key create: %v", provider, err)
	}

	respKey := key
	respKey.Value = *key.Value.Redacted()
	SendJSON(ctx, respKey)
}

func (h *ProviderHandler) updateProviderKey(ctx *fasthttp.RequestCtx) {
	defer EvictTenant(ctx) // see providers.addProvider for rationale

	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}

	keyID, err := getKeyIDFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	var updateKey schemas.Key
	if err := sonic.Unmarshal(ctx.PostBody(), &updateKey); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}

	if h.dbStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "Config store not configured")
		return
	}
	providerConfig, err := h.dbStore.GetProviderConfig(ctx, provider)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider config: %v", err))
		return
	}

	if providerConfig.CustomProviderConfig != nil && providerConfig.CustomProviderConfig.IsKeyLess {
		SendError(ctx, fasthttp.StatusBadRequest, "Cannot update keys on a keyless provider")
		return
	}

	oldRawKey, err := h.dbStore.GetProviderKey(ctx, provider, keyID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider key not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider key: %v", err))
		return
	}
	oldRedactedKey := *oldRawKey
	oldRedactedKey.Value = *oldRawKey.Value.Redacted()

	updateKey.ID = keyID
	mergedKey := h.mergeUpdatedKey(*oldRawKey, oldRedactedKey, updateKey)

	baseProvider := provider
	if providerConfig.CustomProviderConfig != nil && providerConfig.CustomProviderConfig.BaseProviderType != "" {
		baseProvider = providerConfig.CustomProviderConfig.BaseProviderType
	}

	if !bifrost.CanProviderKeyValueBeEmpty(baseProvider) && mergedKey.Value.GetValue() == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Key value must not be empty")
		return
	}

	if err := mergedKey.BlacklistedModels.Validate(); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid blacklisted_models: %v", err))
		return
	}

	if err := mergedKey.Aliases.Validate(); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid aliases: %v", err))
		return
	}

	if err := validateProviderKeyURL(provider, mergedKey); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if err := h.dbStore.UpdateProviderKey(ctx, provider, keyID, mergedKey); err != nil {
		logger.Warn("Failed to update key %s for provider %s: %v", keyID, provider, err)
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider key not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update provider key: %v", err))
		return
	}

	// Best-effort in-memory sync so the shared inference runtime picks
	// up the rename / value rotation without a restart.
	h.inMemoryStore.Mu.Lock()
	if cfg, ok := h.inMemoryStore.Providers[provider]; ok {
		for i, k := range cfg.Keys {
			if k.ID == keyID {
				cfg.Keys[i] = mergedKey
				break
			}
		}
		h.inMemoryStore.Providers[provider] = cfg
	}
	h.inMemoryStore.Mu.Unlock()

	if err := h.attemptModelDiscovery(ctx, provider, providerConfig.CustomProviderConfig); err != nil {
		logger.Warn("Model discovery failed for provider %s after key update: %v", provider, err)
	}

	respKey := mergedKey
	respKey.Value = *mergedKey.Value.Redacted()
	SendJSON(ctx, respKey)
}

func (h *ProviderHandler) deleteProviderKey(ctx *fasthttp.RequestCtx) {
	defer EvictTenant(ctx) // see providers.addProvider for rationale

	provider, err := getProviderFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid provider: %v", err))
		return
	}

	keyID, err := getKeyIDFromCtx(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if h.dbStore == nil {
		SendError(ctx, fasthttp.StatusServiceUnavailable, "Config store not configured")
		return
	}

	// Tenant-scoped lookup: GORM scope callback adds WHERE tenant_id = ?
	// from the request ctx, so a key visible in the global in-memory map
	// but stamped with a different tenant_id returns ErrNotFound here —
	// the correct multi-tenant behaviour. Mirrors the fix applied to
	// addProvider in this same pass.
	providerConfig, err := h.dbStore.GetProviderConfig(ctx, provider)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider config: %v", err))
		return
	}

	if providerConfig.CustomProviderConfig != nil && providerConfig.CustomProviderConfig.IsKeyLess {
		SendError(ctx, fasthttp.StatusBadRequest, "Cannot delete keys on a keyless provider")
		return
	}

	rawKey, err := h.dbStore.GetProviderKey(ctx, provider, keyID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider key not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get provider key: %v", err))
		return
	}
	redactedKey := *rawKey
	redactedKey.Value = *rawKey.Value.Redacted()

	if err := h.dbStore.DeleteProviderKey(ctx, provider, keyID); err != nil {
		logger.Warn("Failed to delete key %s for provider %s: %v", keyID, provider, err)
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Provider key not found: %v", err))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete provider key: %v", err))
		return
	}

	// Best-effort in-memory cleanup so the shared inference runtime
	// drops the key from its rotation pool until Stage 2 replaces the
	// global c.Providers map with a tenant-keyed one.
	h.inMemoryStore.Mu.Lock()
	if cfg, ok := h.inMemoryStore.Providers[provider]; ok {
		filtered := cfg.Keys[:0]
		for _, k := range cfg.Keys {
			if k.ID != keyID {
				filtered = append(filtered, k)
			}
		}
		cfg.Keys = filtered
		h.inMemoryStore.Providers[provider] = cfg
	}
	h.inMemoryStore.Mu.Unlock()

	if err := h.attemptModelDiscovery(ctx, provider, providerConfig.CustomProviderConfig); err != nil {
		logger.Warn("Model discovery failed for provider %s after key delete: %v", provider, err)
	}

	SendJSON(ctx, redactedKey)
}

// mergeUpdatedKey merges an updated key with the old raw/redacted versions,
// preserving real values for fields that were sent back in redacted form.
func (h *ProviderHandler) mergeUpdatedKey(oldRawKey, oldRedactedKey, updateKey schemas.Key) schemas.Key {
	mergedKey := updateKey

	if updateKey.Value.IsRedacted() && updateKey.Value.Equals(&oldRedactedKey.Value) {
		mergedKey.Value = oldRawKey.Value
	}

	if updateKey.AzureKeyConfig != nil && oldRedactedKey.AzureKeyConfig != nil && oldRawKey.AzureKeyConfig != nil {
		if updateKey.AzureKeyConfig.Endpoint.IsRedacted() &&
			updateKey.AzureKeyConfig.Endpoint.Equals(&oldRedactedKey.AzureKeyConfig.Endpoint) {
			mergedKey.AzureKeyConfig.Endpoint = oldRawKey.AzureKeyConfig.Endpoint
		}
		if updateKey.AzureKeyConfig.ClientID != nil &&
			oldRedactedKey.AzureKeyConfig.ClientID != nil &&
			oldRawKey.AzureKeyConfig != nil &&
			updateKey.AzureKeyConfig.ClientID.IsRedacted() &&
			updateKey.AzureKeyConfig.ClientID.Equals(oldRedactedKey.AzureKeyConfig.ClientID) {
			mergedKey.AzureKeyConfig.ClientID = oldRawKey.AzureKeyConfig.ClientID
		}
		if updateKey.AzureKeyConfig.ClientSecret != nil &&
			oldRedactedKey.AzureKeyConfig.ClientSecret != nil &&
			oldRawKey.AzureKeyConfig != nil &&
			updateKey.AzureKeyConfig.ClientSecret.IsRedacted() &&
			updateKey.AzureKeyConfig.ClientSecret.Equals(oldRedactedKey.AzureKeyConfig.ClientSecret) {
			mergedKey.AzureKeyConfig.ClientSecret = oldRawKey.AzureKeyConfig.ClientSecret
		}
		if updateKey.AzureKeyConfig.TenantID != nil &&
			oldRedactedKey.AzureKeyConfig.TenantID != nil &&
			oldRawKey.AzureKeyConfig != nil &&
			updateKey.AzureKeyConfig.TenantID.IsRedacted() &&
			updateKey.AzureKeyConfig.TenantID.Equals(oldRedactedKey.AzureKeyConfig.TenantID) {
			mergedKey.AzureKeyConfig.TenantID = oldRawKey.AzureKeyConfig.TenantID
		}
	}

	if updateKey.VertexKeyConfig != nil && oldRedactedKey.VertexKeyConfig != nil && oldRawKey.VertexKeyConfig != nil {
		if updateKey.VertexKeyConfig.ProjectID.IsRedacted() &&
			updateKey.VertexKeyConfig.ProjectID.Equals(&oldRedactedKey.VertexKeyConfig.ProjectID) {
			mergedKey.VertexKeyConfig.ProjectID = oldRawKey.VertexKeyConfig.ProjectID
		}
		if updateKey.VertexKeyConfig.ProjectNumber.IsRedacted() &&
			updateKey.VertexKeyConfig.ProjectNumber.Equals(&oldRedactedKey.VertexKeyConfig.ProjectNumber) {
			mergedKey.VertexKeyConfig.ProjectNumber = oldRawKey.VertexKeyConfig.ProjectNumber
		}
		if updateKey.VertexKeyConfig.Region.IsRedacted() &&
			updateKey.VertexKeyConfig.Region.Equals(&oldRedactedKey.VertexKeyConfig.Region) {
			mergedKey.VertexKeyConfig.Region = oldRawKey.VertexKeyConfig.Region
		}
		if updateKey.VertexKeyConfig.AuthCredentials.IsRedacted() &&
			updateKey.VertexKeyConfig.AuthCredentials.Equals(&oldRedactedKey.VertexKeyConfig.AuthCredentials) {
			mergedKey.VertexKeyConfig.AuthCredentials = oldRawKey.VertexKeyConfig.AuthCredentials
		}
	}

	if updateKey.BedrockKeyConfig != nil && oldRedactedKey.BedrockKeyConfig != nil && oldRawKey.BedrockKeyConfig != nil {
		if updateKey.BedrockKeyConfig.AccessKey.IsRedacted() &&
			updateKey.BedrockKeyConfig.AccessKey.Equals(&oldRedactedKey.BedrockKeyConfig.AccessKey) {
			mergedKey.BedrockKeyConfig.AccessKey = oldRawKey.BedrockKeyConfig.AccessKey
		}
		if updateKey.BedrockKeyConfig.SecretKey.IsRedacted() &&
			updateKey.BedrockKeyConfig.SecretKey.Equals(&oldRedactedKey.BedrockKeyConfig.SecretKey) {
			mergedKey.BedrockKeyConfig.SecretKey = oldRawKey.BedrockKeyConfig.SecretKey
		}
		if updateKey.BedrockKeyConfig.SessionToken != nil &&
			oldRedactedKey.BedrockKeyConfig.SessionToken != nil &&
			oldRawKey.BedrockKeyConfig != nil &&
			updateKey.BedrockKeyConfig.SessionToken.IsRedacted() &&
			updateKey.BedrockKeyConfig.SessionToken.Equals(oldRedactedKey.BedrockKeyConfig.SessionToken) {
			mergedKey.BedrockKeyConfig.SessionToken = oldRawKey.BedrockKeyConfig.SessionToken
		}
		if updateKey.BedrockKeyConfig.Region != nil &&
			oldRedactedKey.BedrockKeyConfig.Region != nil &&
			oldRawKey.BedrockKeyConfig != nil &&
			updateKey.BedrockKeyConfig.Region.IsRedacted() &&
			updateKey.BedrockKeyConfig.Region.Equals(oldRedactedKey.BedrockKeyConfig.Region) {
			mergedKey.BedrockKeyConfig.Region = oldRawKey.BedrockKeyConfig.Region
		}
		if updateKey.BedrockKeyConfig.ARN != nil &&
			oldRedactedKey.BedrockKeyConfig.ARN != nil &&
			oldRawKey.BedrockKeyConfig != nil &&
			updateKey.BedrockKeyConfig.ARN.IsRedacted() &&
			updateKey.BedrockKeyConfig.ARN.Equals(oldRedactedKey.BedrockKeyConfig.ARN) {
			mergedKey.BedrockKeyConfig.ARN = oldRawKey.BedrockKeyConfig.ARN
		}
		if updateKey.BedrockKeyConfig.RoleARN != nil &&
			oldRedactedKey.BedrockKeyConfig.RoleARN != nil &&
			oldRawKey.BedrockKeyConfig != nil &&
			updateKey.BedrockKeyConfig.RoleARN.IsRedacted() &&
			updateKey.BedrockKeyConfig.RoleARN.Equals(oldRedactedKey.BedrockKeyConfig.RoleARN) {
			mergedKey.BedrockKeyConfig.RoleARN = oldRawKey.BedrockKeyConfig.RoleARN
		}
		if updateKey.BedrockKeyConfig.ExternalID != nil &&
			oldRedactedKey.BedrockKeyConfig.ExternalID != nil &&
			oldRawKey.BedrockKeyConfig != nil &&
			updateKey.BedrockKeyConfig.ExternalID.IsRedacted() &&
			updateKey.BedrockKeyConfig.ExternalID.Equals(oldRedactedKey.BedrockKeyConfig.ExternalID) {
			mergedKey.BedrockKeyConfig.ExternalID = oldRawKey.BedrockKeyConfig.ExternalID
		}
		if updateKey.BedrockKeyConfig.RoleSessionName != nil &&
			oldRedactedKey.BedrockKeyConfig.RoleSessionName != nil &&
			oldRawKey.BedrockKeyConfig != nil &&
			updateKey.BedrockKeyConfig.RoleSessionName.IsRedacted() &&
			updateKey.BedrockKeyConfig.RoleSessionName.Equals(oldRedactedKey.BedrockKeyConfig.RoleSessionName) {
			mergedKey.BedrockKeyConfig.RoleSessionName = oldRawKey.BedrockKeyConfig.RoleSessionName
		}
	}

	if updateKey.VLLMKeyConfig != nil && oldRedactedKey.VLLMKeyConfig != nil && oldRawKey.VLLMKeyConfig != nil {
		if updateKey.VLLMKeyConfig.URL.IsRedacted() &&
			updateKey.VLLMKeyConfig.URL.Equals(&oldRedactedKey.VLLMKeyConfig.URL) {
			mergedKey.VLLMKeyConfig.URL = oldRawKey.VLLMKeyConfig.URL
		}
	}

	// ReplicateKeyConfig has no sensitive fields — pass through as-is
	if updateKey.ReplicateKeyConfig == nil && oldRawKey.ReplicateKeyConfig != nil {
		mergedKey.ReplicateKeyConfig = oldRawKey.ReplicateKeyConfig
	}

	if updateKey.OllamaKeyConfig != nil && oldRedactedKey.OllamaKeyConfig != nil && oldRawKey.OllamaKeyConfig != nil {
		if updateKey.OllamaKeyConfig.URL.IsRedacted() &&
			updateKey.OllamaKeyConfig.URL.Equals(&oldRedactedKey.OllamaKeyConfig.URL) {
			mergedKey.OllamaKeyConfig.URL = oldRawKey.OllamaKeyConfig.URL
		}
	}

	if updateKey.SGLKeyConfig != nil && oldRedactedKey.SGLKeyConfig != nil && oldRawKey.SGLKeyConfig != nil {
		if updateKey.SGLKeyConfig.URL.IsRedacted() &&
			updateKey.SGLKeyConfig.URL.Equals(&oldRedactedKey.SGLKeyConfig.URL) {
			mergedKey.SGLKeyConfig.URL = oldRawKey.SGLKeyConfig.URL
		}
	}

	mergedKey.ConfigHash = oldRawKey.ConfigHash
	mergedKey.Status = oldRawKey.Status

	return mergedKey
}

func getKeyIDFromCtx(ctx *fasthttp.RequestCtx) (string, error) {
	keyValue := ctx.UserValue("key_id")
	if keyValue == nil {
		return "", fmt.Errorf("missing key_id parameter")
	}

	keyID, ok := keyValue.(string)
	if !ok || keyID == "" {
		return "", fmt.Errorf("invalid key_id parameter")
	}

	decoded, err := url.PathUnescape(keyID)
	if err != nil {
		return "", fmt.Errorf("invalid key_id parameter encoding: %v", err)
	}

	return decoded, nil
}

// validateProviderKeyURL checks that Ollama/SGL keys have a server URL configured.
func validateProviderKeyURL(provider schemas.ModelProvider, key schemas.Key) error {
	switch provider {
	case schemas.Ollama:
		if key.OllamaKeyConfig == nil || !key.OllamaKeyConfig.URL.IsSet() {
			return fmt.Errorf("ollama_key_config.url is required for Ollama keys")
		}
	case schemas.SGL:
		if key.SGLKeyConfig == nil || !key.SGLKeyConfig.URL.IsSet() {
			return fmt.Errorf("sgl_key_config.url is required for SGL keys")
		}
	}
	return nil
}
