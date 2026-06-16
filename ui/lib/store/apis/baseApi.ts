import { IS_ENTERPRISE } from "@/lib/constants/config";
import { BifrostErrorResponse } from "@/lib/types/config";
import { getApiBaseUrl } from "@/lib/utils/port";
import { createBaseQueryWithRefresh } from "@enterprise/lib/store/utils/baseQueryWithRefresh";
import { clearOAuthStorage } from "@enterprise/lib/store/utils/tokenManager";
import { createApi, fetchBaseQuery } from "@reduxjs/toolkit/query/react";
import { getActiveTempToken, getSuppressGlobal401 } from "./tempToken";

// Auth tokens are now stored in HTTP-only cookies (set by server)
// No client-side token needed — handled by credentials: "include"
export const getTokenFromStorage = (): Promise<string | null> => {
  return Promise.resolve(null);
};

// Helper function to set auth token
// Non-enterprise: no-op — auth relies on HTTPOnly cookies set by the server
// Enterprise: handled separately via tokenManager
export const setAuthToken = (_token: string | null) => {
  // Non-enterprise auth is cookie-based; no client-side token storage needed.
  // Enterprise token management is handled by the tokenManager module.
};

// Helper function to clear all auth-related storage
export const clearAuthStorage = () => {
  if (typeof window === "undefined") {
    return;
  }
  try {
    // Clear traditional auth token
    localStorage.removeItem("bifrost-auth-token");

    // Clear enterprise OAuth tokens using tokenManager
    if (IS_ENTERPRISE) {
      clearOAuthStorage();
    }
  } catch (error) {
    console.error("Error clearing auth storage:", error);
  }
};

// Define the base query with authentication headers
const baseQuery = fetchBaseQuery({
	baseUrl: getApiBaseUrl(),
	credentials: "include",
	prepareHeaders: async (headers) => {
    if (!headers.has("Content-Type")) {
      headers.set("Content-Type", "application/json");
    }
		// Automatically include token from localStorage in Authorization header
		const token = await getTokenFromStorage();
		if (token) {
			headers.set("Authorization", `Bearer ${token}`);
		}
		// Attach a temp token when a TempTokenScope wrapper is mounted. The
		// dashboard cookie (if present) still takes precedence on the server
		// side; the temp token is the fallback that rescues unauthenticated
		// browsers visiting a scoped page.
		const tempToken = getActiveTempToken();
		if (tempToken) {
			headers.set("X-Bifrost-Temp-Token", tempToken);
		}
		return headers;
	},
});

// TENANT_SCOPED_PREFIXES enumerates the per-tenant admin surfaces the
// F5XC patches added to bifrost-http. Endpoint definitions across the
// UI keep their legacy single-tenant paths (e.g. "/providers",
// "/governance/virtual-keys"); the wrapper below rewrites those to
// "/tenants/{tid}/..." at request time when an active tenant id is set
// in Redux. That way the existing 30+ endpoint files don't need to
// thread tenantID through every call site.
//
// Order matters: longer-prefix matches must come first so e.g. an
// inference path "/v1/..." (no prefix) doesn't get confused with an
// admin "/v1/..." were we to add one later. Today every entry here is
// a /api-relative admin path, distinct from /v1 inference.
const TENANT_SCOPED_PREFIXES = [
	"/providers",
	"/governance/virtual-keys",
	"/governance/teams",
	"/governance/customers",
	"/governance/budgets",
	"/governance/rate-limits",
	"/mcp/clients",
];

// TENANT_QUERY_PARAM_PREFIXES enumerates URL prefixes whose endpoints
// take a `tenant_id` QUERY PARAM (not a path segment). The dashboard
// logs / analytics endpoints live under /api/logs — they're shared
// admin endpoints whose results we want per-tenant filtered when a
// tenant is active. The backend's parseHistogramFilters reads
// `tenant_id` and applies a WHERE clause on logs.tenant_id. Multi-
// value support comes for free via comma-separated values.
const TENANT_QUERY_PARAM_PREFIXES = ["/logs"];

// applyTenantPrefix rewrites a URL string (path + optional query) into
// the tenant-scoped form when (a) a tenant is currently selected in
// Redux and (b) the path matches one of the tenant-scoped prefixes.
// Returns the input unchanged otherwise.
function applyTenantPrefix(rawURL: string, tenantID: string | null): string {
	if (!tenantID) return rawURL;
	// fetchBaseQuery passes the endpoint path WITHOUT the baseUrl. It can
	// be either with or without a leading slash. Normalize so the prefix
	// match works either way.
	const leadingSlash = rawURL.startsWith("/");
	const path = leadingSlash ? rawURL : `/${rawURL}`;
	for (const prefix of TENANT_SCOPED_PREFIXES) {
		if (path === prefix || path.startsWith(`${prefix}/`) || path.startsWith(`${prefix}?`)) {
			const rewritten = `/tenants/${encodeURIComponent(tenantID)}${path}`;
			return leadingSlash ? rewritten : rewritten.slice(1);
		}
	}
	// Query-param tenant scoping (logs / dashboard analytics). Append
	// `tenant_id=<id>` to the existing query string when the caller
	// didn't already supply one — explicit caller-provided tenant_id
	// (e.g. an admin-wide multi-tenant view) wins.
	for (const prefix of TENANT_QUERY_PARAM_PREFIXES) {
		if (path === prefix || path.startsWith(`${prefix}/`) || path.startsWith(`${prefix}?`)) {
			const qIndex = path.indexOf("?");
			const hasQuery = qIndex >= 0;
			const queryStr = hasQuery ? path.slice(qIndex + 1) : "";
			if (/(?:^|&)tenant_id=/.test(queryStr)) {
				return rawURL;
			}
			const sep = hasQuery && queryStr.length > 0 ? "&" : "?";
			const appended = `${path}${sep}tenant_id=${encodeURIComponent(tenantID)}`;
			return leadingSlash ? appended : appended.slice(1);
		}
	}
	return rawURL;
}

// baseQueryWithTenantPrefix wraps fetchBaseQuery so every outgoing
// admin request automatically picks up /tenants/{tid}/... when a
// tenant is active. Endpoint definitions stay agnostic.
const baseQueryWithTenantPrefix: typeof baseQuery = (args, api, extraOptions) => {
	const state = api.getState() as { tenant?: { currentTenantID: string | null } };
	const tenantID = state.tenant?.currentTenantID ?? null;
	if (typeof args === "string") {
		return baseQuery(applyTenantPrefix(args, tenantID), api, extraOptions);
	}
	if (args && typeof args === "object" && "url" in args && typeof args.url === "string") {
		return baseQuery({ ...args, url: applyTenantPrefix(args.url, tenantID) }, api, extraOptions);
	}
	return baseQuery(args, api, extraOptions);
};

// Wrap base query with enterprise refresh logic (or passthrough for non-enterprise)
const baseQueryWithRefresh = createBaseQueryWithRefresh(baseQueryWithTenantPrefix);

// Enhanced base query with error handling
const baseQueryWithErrorHandling: typeof baseQueryWithRefresh = async (
  args: any,
  api: any,
  extraOptions: any,
) => {
  // First apply refresh logic (enterprise-specific, handles 401)
  const result = await baseQueryWithRefresh(args, api, extraOptions);

  // Then handle other error types
  if (result.error) {
    const error = result.error as any;

		// Handle 401 for non-enterprise (no refresh available)
		if (error?.status === 401 && !IS_ENTERPRISE) {
			// When a TempTokenScope wrapper is active, the wrapped page handles
			// its own 401 display (an "invalid/expired link" view). Skip the
			// global redirect so the user stays on the page they opened.
			if (getSuppressGlobal401()) {
				return result;
			}
			clearAuthStorage();
			if (typeof window !== "undefined" && !window.location.pathname.includes("/login")) {
				window.location.href = "/login";
			}
			return result;
		}

    // Handle specific error types
    if (error?.status === "FETCH_ERROR") {
      // Network error
      return {
        ...result,
        error: {
          ...error,
          data: {
            error: {
              message: "Network error: Unable to connect to the server",
            },
          },
        },
      };
    }

    // Handle other errors with proper BifrostErrorResponse format
    if (error?.data) {
      const errorData = error.data as BifrostErrorResponse;
      if (errorData.error?.message) {
        return result;
      }
    }

    // Fallback error message
    return {
      ...result,
      error: {
        ...error,
        data: {
          error: {
            message: "An unexpected error occurred",
          },
        },
      },
    };
  }

  return result;
};

// Create the base API
export const baseApi = createApi({
  reducerPath: "api",
  baseQuery: baseQueryWithErrorHandling,
  tagTypes: [
    "Logs",
    "MCPLogs",
    "Providers",
    "MCPClients",
    "Config",
    "CacheConfig",
    "VirtualKeys",
    "Teams",
    "Customers",
    "Budgets",
    "RateLimits",
    "UsageStats",
    "DebugStats",
    "HealthCheck",
    "DBKeys",
    "ProviderKeys",
    "Models",
    "BaseModels",
    "ModelConfigs",
    "ProviderGovernance",
    "Plugins",
    "SCIMProviders",
    "User",
    "Guardrails",
    "ClusterNodes",
    "Users",
    "GuardrailRules",
    "Roles",
    "Resources",
    "Operations",
    "Permissions",
    "APIKeys",
    "OAuth2Config",
    "RoutingRules",
    "PricingOverrides",
    "MCPToolGroups",
    "AuditLogs",
    "UserGovernance",
    "LargePayloadConfig",
    "Folders",
    "Prompts",
    "Versions",
    "Sessions",
    "AccessProfiles",
    "AccessProfileVirtualKeys",
    "BusinessUnits",
    "PromptDeployments",
    "AuthType",
    "MCPSessions",
    "FeatureFlags",
    "Tenants",
  ],
  endpoints: () => ({}),
});

// Helper function to extract error message from RTK Query error
export const getErrorMessage = (error: unknown): string => {
  if (error === undefined || error === null) {
    return "An unexpected error occurred";
  }
  if (error instanceof Error) {
    return error.message;
  }
  if (
    typeof error === "object" &&
    error &&
    "data" in error &&
    error.data &&
    typeof error.data === "object" &&
    "error" in error.data &&
    error.data.error &&
    typeof error.data.error === "object" &&
    "message" in error.data.error &&
    typeof error.data.error.message === "string"
  ) {
    return (
      error.data.error.message.charAt(0).toUpperCase() +
      error.data.error.message.slice(1)
    );
  }
  if (
    typeof error === "object" &&
    error &&
    "message" in error &&
    typeof error.message === "string"
  ) {
    return error.message;
  }
  return "An unexpected error occurred";
};
