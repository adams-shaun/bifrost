import { useAppDispatch, useAppSelector } from "../hooks";
import { setCurrentTenant, clearTenant } from "../slices/tenantSlice";

// useCurrentTenant is the canonical hook every multi-tenant-aware
// component should reach for. Returns the current tenant id (or null)
// and setter/clearer actions that automatically persist to localStorage
// via the slice's reducer.
//
// Returning `null` is a deliberate signal — "no tenant selected yet".
// Components should NOT default to "default" silently; either render a
// "pick a tenant" prompt or skip the query. The login page is the only
// place that normalizes null → a real tenant id.
export function useCurrentTenant() {
	const dispatch = useAppDispatch();
	const currentTenantID = useAppSelector((s) => s.tenant.currentTenantID);
	return {
		currentTenantID,
		setCurrentTenant: (id: string | null) => dispatch(setCurrentTenant(id)),
		clearTenant: () => dispatch(clearTenant()),
	};
}
