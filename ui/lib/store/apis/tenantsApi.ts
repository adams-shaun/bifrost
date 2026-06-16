import { baseApi } from "./baseApi";

// Tenant is the minimal shape returned by GET /api/platform/tenants —
// only the fields the UI surfaces (id for selection, name for display,
// status for filtering inactive). The full TableTenant has more fields
// (description, timestamps, etc.); add them here as views need them.
export interface Tenant {
	id: string;
	name: string;
	status: "active" | "suspended" | string;
	description?: string;
	created_at?: string;
	updated_at?: string;
}

export interface ListTenantsResponse {
	tenants: Tenant[];
	count: number;
	total_count: number;
	limit: number;
	offset: number;
}

export interface CreateTenantRequest {
	id: string;
	name: string;
	status?: "active" | "suspended";
	description?: string;
}

export interface CreateTenantResponse {
	message: string;
	tenant: Tenant;
}

// tenantsApi wraps two endpoints:
//   - GET /api/session/tenants  — public, minimal {id, name, status},
//     callable BEFORE login (used by the login page tenant picker).
//   - GET /api/platform/tenants — auth-required, full metadata
//     including description + timestamps (used by the in-workspace
//     switcher / admin tenant CRUD).
// Today the AdminAuthz stub treats every authenticated caller as a
// platform admin, so any logged-in user sees the full list. Phase 4
// adds an OIDC-backed AuthzPlugin that restricts visibility per caller;
// the UI's tenant picker will then show only the tenants the caller
// has any role on.
export const tenantsApi = baseApi.injectEndpoints({
	endpoints: (builder) => ({
		// List tenants (admin endpoint). Used by the in-workspace
		// switcher where the caller is already authenticated. The
		// returned objects carry the full Tenant fields.
		getTenants: builder.query<Tenant[], { search?: string; status?: string } | void>({
			query: (params) => {
				const sp = new URLSearchParams();
				sp.set("limit", "1000");
				if (params?.search) sp.set("search", params.search);
				if (params?.status) sp.set("status", params.status);
				return `/platform/tenants?${sp.toString()}`;
			},
			transformResponse: (response: ListTenantsResponse): Tenant[] =>
				(response.tenants ?? []).slice().sort((a, b) => a.name.localeCompare(b.name)),
			providesTags: ["Tenants"],
		}),

		// Public tenant directory for the login picker. Returns only
		// {id, name, status} — strictly less than getTenants — so
		// exposing it pre-auth doesn't leak admin metadata. Backed by
		// /api/session/tenants which is on the auth bypass list.
		getTenantsPublic: builder.query<Tenant[], void>({
			query: () => "/session/tenants",
			transformResponse: (response: { tenants: Tenant[] }): Tenant[] =>
				(response.tenants ?? []).slice().sort((a, b) => a.name.localeCompare(b.name)),
			providesTags: ["Tenants"],
		}),

		// Single tenant by id. Used by the in-app switcher to verify a
		// localStorage-cached tenant still exists before honoring it.
		getTenant: builder.query<Tenant, string>({
			query: (id) => `/platform/tenants/${encodeURIComponent(id)}`,
			transformResponse: (response: { tenant: Tenant }) => response.tenant,
			providesTags: (_result, _err, id) => [{ type: "Tenants", id }],
		}),

		// Create a new tenant. Surfaced from a "+ New tenant" affordance on
		// the picker once the basic flow lands.
		createTenant: builder.mutation<CreateTenantResponse, CreateTenantRequest>({
			query: (body) => ({
				url: "/platform/tenants",
				method: "POST",
				body,
			}),
			invalidatesTags: ["Tenants"],
		}),
	}),
});

export const { useGetTenantsQuery, useGetTenantsPublicQuery, useGetTenantQuery, useCreateTenantMutation } = tenantsApi;
