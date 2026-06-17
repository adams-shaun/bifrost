import { baseApi } from "./baseApi";

// Tenant is the shape returned by GET /api/platform/tenants — the
// fields the UI surfaces (id for selection, name for display, status
// for filtering inactive).  The full TableTenant has more fields
// (timestamps, etc.); add them here as views need them.
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

// tenantsApi wraps the /api/platform/tenants admin surface.  All
// endpoints are auth-gated — the user logs in normally, lands in the
// workspace, and the tenant badge fetches the list to drive the
// switcher dropdown.  There's no pre-login tenant picker in the
// header model: the request header is the source of truth and
// defaults to "default" until the user picks otherwise.
export const tenantsApi = baseApi.injectEndpoints({
	endpoints: (builder) => ({
		getTenants: builder.query<Tenant[], void>({
			query: () => "/tenants",
			transformResponse: (response: ListTenantsResponse): Tenant[] =>
				(response.tenants ?? []).slice().sort((a, b) => a.name.localeCompare(b.name)),
			providesTags: ["Tenants"],
		}),

		getTenant: builder.query<Tenant, string>({
			query: (id) => `/tenants/${encodeURIComponent(id)}`,
			transformResponse: (response: { tenant: Tenant }) => response.tenant,
			providesTags: (_result, _err, id) => [{ type: "Tenants", id }],
		}),

		createTenant: builder.mutation<CreateTenantResponse, CreateTenantRequest>({
			query: (body) => ({
				url: "/tenants",
				method: "POST",
				body,
			}),
			invalidatesTags: ["Tenants"],
		}),
	}),
});

export const { useGetTenantsQuery, useGetTenantQuery, useCreateTenantMutation } = tenantsApi;
