import { createSlice, PayloadAction } from "@reduxjs/toolkit";

// LocalStorage key for the active tenant id. SSR-safe (we only read in
// the browser; window is guarded).
const STORAGE_KEY = "bifrost-current-tenant-id";

// hydrateInitial reads the persisted tenant id at slice-init time. Wrap
// localStorage access in try/catch so a quota / disabled-storage error
// degrades to no-tenant rather than crashing the whole app on boot.
function hydrateInitial(): string | null {
	if (typeof window === "undefined") return null;
	try {
		const v = window.localStorage.getItem(STORAGE_KEY);
		return v && v.trim() !== "" ? v : null;
	} catch {
		return null;
	}
}

export interface TenantState {
	// currentTenantID is null until the user picks one (or until login
	// hydration runs). Components that need a tenant should treat null
	// as "no tenant in scope" and short-circuit their queries.
	currentTenantID: string | null;
}

const initialState: TenantState = {
	currentTenantID: hydrateInitial(),
};

const tenantSlice = createSlice({
	name: "tenant",
	initialState,
	reducers: {
		// setCurrentTenant persists to localStorage as a side effect so a
		// page reload preserves the selection. The persist failure is
		// intentionally silent — the in-memory state already reflects the
		// user's choice; losing it on reload is a graceful degradation.
		setCurrentTenant(state, action: PayloadAction<string | null>) {
			state.currentTenantID = action.payload;
			if (typeof window !== "undefined") {
				try {
					if (action.payload) {
						window.localStorage.setItem(STORAGE_KEY, action.payload);
					} else {
						window.localStorage.removeItem(STORAGE_KEY);
					}
				} catch {
					// best-effort
				}
			}
		},
		clearTenant(state) {
			state.currentTenantID = null;
			if (typeof window !== "undefined") {
				try {
					window.localStorage.removeItem(STORAGE_KEY);
				} catch {
					// best-effort
				}
			}
		},
	},
});

export const { setCurrentTenant, clearTenant } = tenantSlice.actions;
export default tenantSlice.reducer;
