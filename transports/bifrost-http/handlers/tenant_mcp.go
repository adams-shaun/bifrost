// Package handlers — tenant-scoped MCP admin.
//
// Mounts POST /api/tenants/{tenant_id}/mcp/clients so platform-admins can
// provision per-tenant MCP servers. The per-tenant Bifrost runtime
// loader assembles BifrostConfig.MCP from GetMCPConfigByTenant, so an
// MCP client created here is only visible to that tenant's
// /v1/mcp/tool/execute calls.
//
// v1 minimum surface: POST only, no OAuth flow, no server-sync. The
// legacy /api/mcp/client at /api/mcp/client supports the full OAuth +
// admin-login + tool-discovery flow; that comes under the tenant-scoped
// route family once Phase 4's AuthzPlugin lands.
package handlers

import (
	"errors"
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// TenantMCPHandler implements POST /api/tenants/{tenant_id}/mcp/clients.
type TenantMCPHandler struct {
	configStore configstore.ConfigStore
	evictor     TenantRuntimeEvictor
}

// NewTenantMCPHandler constructs a TenantMCPHandler.
func NewTenantMCPHandler(configStore configstore.ConfigStore, evictor TenantRuntimeEvictor) (*TenantMCPHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &TenantMCPHandler{configStore: configStore, evictor: evictor}, nil
}

// RegisterRoutes mounts the tenant-scoped MCP admin routes. middlewares
// MUST include RequireTenantPathMiddleware (or equivalent).
func (h *TenantMCPHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	if h.configStore == nil {
		return
	}
	r.GET("/api/tenants/{tenant_id}/mcp/clients", lib.ChainMiddlewares(h.listMCPClients, middlewares...))
	r.POST("/api/tenants/{tenant_id}/mcp/clients", lib.ChainMiddlewares(h.createMCPClient, middlewares...))
	r.GET("/api/tenants/{tenant_id}/mcp/clients/{client_id}", lib.ChainMiddlewares(h.getMCPClient, middlewares...))
	r.PUT("/api/tenants/{tenant_id}/mcp/clients/{client_id}", lib.ChainMiddlewares(h.updateMCPClient, middlewares...))
	r.DELETE("/api/tenants/{tenant_id}/mcp/clients/{client_id}", lib.ChainMiddlewares(h.deleteMCPClient, middlewares...))
}

// UpdateTenantMCPClientRequest is the v1 minimum-viable PUT body.
// Lets operators flip the disabled flag (drain a tenant's tool flow
// without removing config) and rotate the connection string. id is
// taken from the URL, not the body.
type UpdateTenantMCPClientRequest struct {
	Disabled         *bool                      `json:"disabled,omitempty"`
	ConnectionString *schemas.EnvVar            `json:"connection_string,omitempty"`
	ConnectionType   *schemas.MCPConnectionType `json:"connection_type,omitempty"`
}

// CreateTenantMCPClientRequest is the minimum-viable payload — enough to
// register an HTTP or STDIO MCP server with no auth. The legacy
// MCPClientRequest under /api/mcp/client supports OAuth, headers,
// allowed-extra-headers, tool-pricing, server-sync, etc.; those can be
// added under the tenant-scoped route family once tenant-admin auth lands.
type CreateTenantMCPClientRequest struct {
	// ClientID is optional; if blank, the server mints a UUID.
	ClientID string `json:"id,omitempty"`
	// Name is required (composite-unique with tenant_id).
	Name string `json:"name"`
	// ConnectionType is one of the schemas.MCPConnectionType values
	// (typically "http" or "stdio"). Required.
	ConnectionType schemas.MCPConnectionType `json:"connection_type"`
	// ConnectionString is the URL (for HTTP) or command (for STDIO).
	// Stored as an EnvVar so the value can come from an env reference.
	ConnectionString *schemas.EnvVar `json:"connection_string,omitempty"`
}

// CreateTenantMCPClientResponse is what we return on success.
type CreateTenantMCPClientResponse struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

func (h *TenantMCPHandler) createMCPClient(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}

	var req CreateTenantMCPClientRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "name is required")
		return
	}
	if req.ConnectionType == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "connection_type is required")
		return
	}
	if req.ClientID == "" {
		req.ClientID = uuid.NewString()
	}

	mcp := &schemas.MCPClientConfig{
		ID:               req.ClientID,
		Name:             req.Name,
		ConnectionType:   req.ConnectionType,
		ConnectionString: req.ConnectionString,
	}

	if err := h.configStore.CreateMCPClientConfigForTenant(ctx, string(tid), mcp); err != nil {
		if errors.Is(err, configstore.ErrAlreadyExists) {
			SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("MCP client %s already exists for tenant %s", req.Name, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create MCP client: %v", err))
		return
	}

	// Evict the per-tenant runtime so the next inference request that
	// hits /v1/mcp/tool/execute for this tenant lazy-loads with the
	// freshly registered MCP client in scope. Without this, the loader's
	// MCP snapshot stays cached for the runtime's lifetime.
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}

	// Return the legacy {status, message} shape so the UI's
	// createMCPClient mutation (typed as the union {status, message} |
	// OAuthFlowResponse) round-trips. Tenant_id remains accessible via
	// the URL the client just POSTed to.
	SendJSONWithStatus(ctx, map[string]any{
		"status":  "success",
		"message": fmt.Sprintf("MCP client %s created for tenant %s", req.Name, tid),
	}, fasthttp.StatusCreated)
}

// listMCPClients — GET /api/tenants/{tenant_id}/mcp/clients
func (h *TenantMCPHandler) listMCPClients(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	cfg, err := h.configStore.GetMCPConfigByTenant(ctx, string(tid))
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to list mcp clients: %v", err))
		return
	}
	clients := []*schemas.MCPClientConfig{}
	if cfg != nil && cfg.ClientConfigs != nil {
		clients = cfg.ClientConfigs
	}
	// Match GetMCPClientsResponse — UI reads response.clients +
	// response.count + total_count. {tenant_id, total} envelope used
	// to leave draft.clients undefined under the type contract.
	SendJSON(ctx, map[string]any{
		"clients":     clients,
		"count":       len(clients),
		"total_count": len(clients),
		"limit":       len(clients),
		"offset":      0,
	})
}

// getMCPClient — GET /api/tenants/{tenant_id}/mcp/clients/{client_id}
func (h *TenantMCPHandler) getMCPClient(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	clientID, _ := ctx.UserValue("client_id").(string)
	if clientID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "client_id is required")
		return
	}
	dbClient, err := h.configStore.GetMCPClientByIDForTenant(ctx, string(tid), clientID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("MCP client %s not found for tenant %s", clientID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get mcp client: %v", err))
		return
	}
	SendJSON(ctx, dbClient)
}

// updateMCPClient — PUT /api/tenants/{tenant_id}/mcp/clients/{client_id}
//
// v1 only flips the safe fields (disabled, connection_string,
// connection_type). Headers/OAuth/tool-pricing/etc. follow the legacy
// /api/mcp/client/{id} when tenant-admin auth lands.
func (h *TenantMCPHandler) updateMCPClient(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	clientID, _ := ctx.UserValue("client_id").(string)
	if clientID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "client_id is required")
		return
	}

	var req UpdateTenantMCPClientRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Disabled == nil && req.ConnectionString == nil && req.ConnectionType == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "at least one of disabled, connection_string, connection_type is required")
		return
	}

	existing, err := h.configStore.GetMCPClientByIDForTenant(ctx, string(tid), clientID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("MCP client %s not found for tenant %s", clientID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load mcp client: %v", err))
		return
	}

	if req.Disabled != nil {
		existing.Disabled = *req.Disabled
	}
	if req.ConnectionString != nil {
		existing.ConnectionString = req.ConnectionString
	}
	if req.ConnectionType != nil {
		existing.ConnectionType = string(*req.ConnectionType)
	}

	if err := h.configStore.UpdateMCPClientConfigForTenant(ctx, string(tid), clientID, existing); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("MCP client %s not found for tenant %s", clientID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update mcp client: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	// UI's updateMCPClient mutation expects {status, message}.
	SendJSON(ctx, map[string]any{
		"status":  "success",
		"message": fmt.Sprintf("MCP client %s updated for tenant %s", clientID, tid),
	})
}

// deleteMCPClient — DELETE /api/tenants/{tenant_id}/mcp/clients/{client_id}
func (h *TenantMCPHandler) deleteMCPClient(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	clientID, _ := ctx.UserValue("client_id").(string)
	if clientID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "client_id is required")
		return
	}
	if err := h.configStore.DeleteMCPClientConfigForTenant(ctx, string(tid), clientID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("MCP client %s not found for tenant %s", clientID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete mcp client: %v", err))
		return
	}
	if h.evictor != nil {
		h.evictor.Evict(tid)
	}
	SendJSON(ctx, map[string]any{
		"status":  "success",
		"message": fmt.Sprintf("MCP client %s deleted for tenant %s", clientID, tid),
	})
}
