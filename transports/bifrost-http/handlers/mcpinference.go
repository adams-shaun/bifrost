package handlers

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

type MCPInferenceHandler struct {
	router lib.BifrostRouter // per-request runtime resolver (single-tenant by default)
	client *bifrost.Bifrost  // legacy back-pointer for hot-reload / shutdown coordination
	config *lib.Config
}

// NewMCPInferenceHandler creates a new MCP inference handler instance
func NewMCPInferenceHandler(client *bifrost.Bifrost, config *lib.Config) *MCPInferenceHandler {
	return &MCPInferenceHandler{
		router: lib.NewSingleTenantRouter(client),
		client: client,
		config: config,
	}
}

// NewMCPInferenceHandlerWithRouter is the router-aware constructor for
// callers that want to inject a multi-tenant BifrostRouter.
func NewMCPInferenceHandlerWithRouter(router lib.BifrostRouter, client *bifrost.Bifrost, config *lib.Config) *MCPInferenceHandler {
	return &MCPInferenceHandler{
		router: router,
		client: client,
		config: config,
	}
}

// RegisterRoutes registers the MCP inference routes
func (h *MCPInferenceHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.POST("/v1/mcp/tool/execute", lib.ChainMiddlewares(h.executeTool, middlewares...))
}

// clientOrFail resolves the per-request Bifrost runtime via the router and
// writes a 500 on acquire failure. Mirrors CompletionHandler.clientOrFail.
func (h *MCPInferenceHandler) clientOrFail(ctx *fasthttp.RequestCtx, bifrostCtx *schemas.BifrostContext) (*bifrost.Bifrost, func(), bool) {
	client, release, err := h.router.Acquire(bifrostCtx)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, fmt.Sprintf("failed to acquire bifrost runtime: %v", err))
		return nil, nil, false
	}
	return client, release, true
}

// executeTool handles POST /v1/mcp/tool/execute - Execute MCP tool
func (h *MCPInferenceHandler) executeTool(ctx *fasthttp.RequestCtx) {
	// Check format query parameter
	format := strings.ToLower(string(ctx.QueryArgs().Peek("format")))
	switch format {
	case "chat", "":
		h.executeChatMCPTool(ctx)
	case "responses":
		h.executeResponsesMCPTool(ctx)
	default:
		SendError(ctx, fasthttp.StatusBadRequest, "Invalid format value, must be 'chat' or 'responses'")
		return
	}
}

// executeChatMCPTool handles POST /v1/mcp/tool/execute?format=chat - Execute MCP tool
func (h *MCPInferenceHandler) executeChatMCPTool(ctx *fasthttp.RequestCtx) {
	var req schemas.ChatAssistantMessageToolCall
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid request format: %v", err))
		return
	}

	// Validate required fields
	if req.Function.Name == nil || *req.Function.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Tool function name is required")
		return
	}

	// Convert context
	bifrostCtx, cancel := lib.ConvertToBifrostContext(ctx, h.config)
	defer cancel() // Ensure cleanup on function exit
	if bifrostCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}

	client, release, ok := h.clientOrFail(ctx, bifrostCtx)
	if !ok {
		return
	}
	defer release()

	// Execute MCP tool
	toolMessage, bifrostErr := client.ExecuteChatMCPTool(bifrostCtx, &req)
	if bifrostErr != nil {
		SendBifrostError(ctx, bifrostErr)
		return
	}

	// Send successful response
	SendJSON(ctx, toolMessage)
}

// executeResponsesMCPTool handles POST /v1/mcp/tool/execute?format=responses - Execute MCP tool
func (h *MCPInferenceHandler) executeResponsesMCPTool(ctx *fasthttp.RequestCtx) {
	var req schemas.ResponsesToolMessage
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid request format: %v", err))
		return
	}

	// Validate required fields
	if req.Name == nil || *req.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Tool function name is required")
		return
	}

	// Convert context
	bifrostCtx, cancel := lib.ConvertToBifrostContext(ctx, h.config)
	defer cancel() // Ensure cleanup on function exit
	if bifrostCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}

	client, release, ok := h.clientOrFail(ctx, bifrostCtx)
	if !ok {
		return
	}
	defer release()

	// Execute MCP tool
	toolMessage, bifrostErr := client.ExecuteResponsesMCPTool(bifrostCtx, &req)
	if bifrostErr != nil {
		SendBifrostError(ctx, bifrostErr)
		return
	}

	// Send successful response
	SendJSON(ctx, toolMessage)
}
