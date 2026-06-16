// Package handlers — tenant-scoped governance team admin.
//
// Mounts CRUD under /api/tenants/{tenant_id}/governance/teams. The
// runtime registry does not lazy-load teams (they're tagging metadata,
// not request-path state), so the evictor pattern from the inference-
// path entities is not wired here. A team change shows up in subsequent
// admin reads + on any VK whose team_id pointer the operator updates.
package handlers

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// TenantTeamsHandler exposes tenant-scoped team CRUD.
type TenantTeamsHandler struct {
	configStore configstore.ConfigStore
}

// NewTenantTeamsHandler constructs a handler. configStore is required.
func NewTenantTeamsHandler(configStore configstore.ConfigStore) (*TenantTeamsHandler, error) {
	if configStore == nil {
		return nil, fmt.Errorf("config store is required")
	}
	return &TenantTeamsHandler{configStore: configStore}, nil
}

// RegisterRoutes mounts the tenant-scoped team admin routes. middlewares
// MUST include RequireTenantPathMiddleware (or equivalent).
func (h *TenantTeamsHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	if h.configStore == nil {
		return
	}
	r.GET("/api/tenants/{tenant_id}/governance/teams", lib.ChainMiddlewares(h.listTeams, middlewares...))
	r.POST("/api/tenants/{tenant_id}/governance/teams", lib.ChainMiddlewares(h.createTeam, middlewares...))
	r.GET("/api/tenants/{tenant_id}/governance/teams/{team_id}", lib.ChainMiddlewares(h.getTeam, middlewares...))
	r.PUT("/api/tenants/{tenant_id}/governance/teams/{team_id}", lib.ChainMiddlewares(h.updateTeam, middlewares...))
	r.DELETE("/api/tenants/{tenant_id}/governance/teams/{team_id}", lib.ChainMiddlewares(h.deleteTeam, middlewares...))
}

// CreateTenantTeamRequest is the POST body. id is optional (UUID minted).
// RateLimit + Budgets mirror the legacy CreateTeamRequest so a team can
// own enforcement state at create time (otherwise the only way to add
// limits was via the rich legacy route, which is single-tenant scoped).
type CreateTenantTeamRequest struct {
	ID              string                  `json:"id,omitempty"`
	Name            string                  `json:"name"`
	CustomerID      *string                 `json:"customer_id,omitempty"`
	CalendarAligned bool                    `json:"calendar_aligned,omitempty"`
	RateLimit       *CreateRateLimitRequest `json:"rate_limit,omitempty"`
	Budgets         []CreateBudgetRequest   `json:"budgets,omitempty"`
}

// UpdateTenantTeamRequest is the PUT body. Pointer fields are optional.
type UpdateTenantTeamRequest struct {
	Name       *string `json:"name,omitempty"`
	CustomerID *string `json:"customer_id,omitempty"`
}

func (h *TenantTeamsHandler) listTeams(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	customerID := string(ctx.QueryArgs().Peek("customer_id"))
	teams, err := h.configStore.GetTeamsByTenant(ctx, string(tid), customerID)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to list teams: %v", err))
		return
	}
	// Match the legacy GetTeamsResponse shape that the UI types
	// against ({teams, count, total_count, limit, offset}); the
	// {tenant_id, teams, total} envelope left draft.teams undefined
	// in the UI's optimistic-update cache patches.
	SendJSON(ctx, map[string]any{
		"teams":       teams,
		"count":       len(teams),
		"total_count": len(teams),
		"limit":       len(teams),
		"offset":      0,
	})
}

func (h *TenantTeamsHandler) createTeam(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	var req CreateTenantTeamRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "name is required")
		return
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}

	// Budget validation up-front so an invalid duration doesn't half-create
	// the team row inside a transaction we then have to roll back.
	if len(req.Budgets) > 0 {
		seen := make(map[string]bool, len(req.Budgets))
		for _, b := range req.Budgets {
			if b.MaxLimit < 0 {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Budget max_limit cannot be negative: %.2f", b.MaxLimit))
				return
			}
			if _, err := configstoreTables.ParseDuration(b.ResetDuration); err != nil {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid reset duration format: %s", b.ResetDuration))
				return
			}
			if seen[b.ResetDuration] {
				SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Duplicate reset_duration in budgets: %s", b.ResetDuration))
				return
			}
			seen[b.ResetDuration] = true
		}
	}

	team := &configstoreTables.TableTeam{
		ID:              req.ID,
		Name:            req.Name,
		CustomerID:      req.CustomerID,
		CalendarAligned: req.CalendarAligned,
	}

	// Minimal POST (no rate_limit / budgets) keeps the direct
	// CreateTeamForTenant path so existing stub-store tests keep working.
	if req.RateLimit == nil && len(req.Budgets) == 0 {
		if err := h.configStore.CreateTeamForTenant(ctx, string(tid), team); err != nil {
			if errors.Is(err, configstore.ErrAlreadyExists) {
				SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Team %s already exists for tenant %s", req.Name, tid))
				return
			}
			SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create team: %v", err))
			return
		}
		SendJSONWithStatus(ctx, map[string]any{
			"message": "Team created successfully",
			"team":    team,
		}, fasthttp.StatusCreated)
		return
	}

	if err := h.configStore.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		// Team-level rate limit
		if req.RateLimit != nil {
			rl := &configstoreTables.TableRateLimit{
				ID:                   uuid.NewString(),
				TokenMaxLimit:        req.RateLimit.TokenMaxLimit,
				TokenResetDuration:   req.RateLimit.TokenResetDuration,
				RequestMaxLimit:      req.RateLimit.RequestMaxLimit,
				RequestResetDuration: req.RateLimit.RequestResetDuration,
				TokenLastReset:       time.Now(),
				RequestLastReset:     time.Now(),
			}
			if err := h.configStore.CreateRateLimitForTenant(ctx, string(tid), rl, tx); err != nil {
				return err
			}
			team.RateLimitID = &rl.ID
		}
		if err := h.configStore.CreateTeamForTenant(ctx, string(tid), team, tx); err != nil {
			return err
		}
		// Team-level multi-budget
		for _, b := range req.Budgets {
			budget := &configstoreTables.TableBudget{
				ID:            uuid.NewString(),
				MaxLimit:      b.MaxLimit,
				ResetDuration: b.ResetDuration,
				LastReset:     budgetLastReset(team.CalendarAligned, b.ResetDuration),
				CurrentUsage:  0,
				TeamID:        &team.ID,
			}
			if err := h.configStore.CreateBudgetForTenant(ctx, string(tid), budget, tx); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, configstore.ErrAlreadyExists) {
			SendError(ctx, fasthttp.StatusConflict, fmt.Sprintf("Team %s already exists for tenant %s", req.Name, tid))
			return
		}
		// Budget / rate-limit input errors bubble up as plain Go errors —
		// surface them as 400 instead of 500 so the UI can show them.
		msg := err.Error()
		if strings.Contains(msg, "rate limit") || strings.Contains(msg, "budget") {
			SendError(ctx, fasthttp.StatusBadRequest, msg)
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to create team: %v", err))
		return
	}
	// {message, team} envelope matches the legacy /api/governance/teams
	// POST so the UI's createTeam mutation (typed as
	// {message: string; team: Team}) round-trips.
	SendJSONWithStatus(ctx, map[string]any{
		"message": "Team created successfully",
		"team":    team,
	}, fasthttp.StatusCreated)
}

func (h *TenantTeamsHandler) getTeam(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	teamID, _ := ctx.UserValue("team_id").(string)
	if teamID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "team_id is required")
		return
	}
	team, err := h.configStore.GetTeamByIDForTenant(ctx, string(tid), teamID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Team %s not found for tenant %s", teamID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to get team: %v", err))
		return
	}
	// UI's getTeam query is typed as {team: Team}.
	SendJSON(ctx, map[string]any{"team": team})
}

func (h *TenantTeamsHandler) updateTeam(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	teamID, _ := ctx.UserValue("team_id").(string)
	if teamID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "team_id is required")
		return
	}
	var req UpdateTenantTeamRequest
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid JSON: %v", err))
		return
	}
	if req.Name == nil && req.CustomerID == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "at least one of name, customer_id is required")
		return
	}
	existing, err := h.configStore.GetTeamByIDForTenant(ctx, string(tid), teamID)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Team %s not found for tenant %s", teamID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to load team: %v", err))
		return
	}
	if req.Name != nil {
		existing.Name = *req.Name
	}
	if req.CustomerID != nil {
		existing.CustomerID = req.CustomerID
	}
	if err := h.configStore.UpdateTeamForTenant(ctx, string(tid), existing); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Team %s not found for tenant %s", teamID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to update team: %v", err))
		return
	}
	// UI's updateTeam mutation is typed as {message, team}.
	SendJSON(ctx, map[string]any{
		"message": "Team updated successfully",
		"team":    existing,
	})
}

func (h *TenantTeamsHandler) deleteTeam(ctx *fasthttp.RequestCtx) {
	tid, ok := tenantIDFromCtx(ctx)
	if !ok {
		SendError(ctx, fasthttp.StatusInternalServerError, "tenant_id missing on ctx — RequireTenantPathMiddleware not mounted?")
		return
	}
	teamID, _ := ctx.UserValue("team_id").(string)
	if teamID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "team_id is required")
		return
	}
	if err := h.configStore.DeleteTeamForTenant(ctx, string(tid), teamID); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			SendError(ctx, fasthttp.StatusNotFound, fmt.Sprintf("Team %s not found for tenant %s", teamID, tid))
			return
		}
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("Failed to delete team: %v", err))
		return
	}
	// UI's deleteTeam mutation is typed as {message: string}.
	SendJSON(ctx, map[string]any{"message": "Team deleted successfully"})
}
