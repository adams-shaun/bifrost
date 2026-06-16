import { configureStore, createListenerMiddleware, isAnyOf } from "@reduxjs/toolkit";
import { baseApi } from "./apis/baseApi";
import { appReducer, pluginReducer, providerReducer, tenantReducer } from "./slices";
import { setCurrentTenant, clearTenant } from "./slices/tenantSlice";
import { reducers as enterpriseReducers, type EnterpriseState } from "@enterprise/lib/store/slices";
// Importing enterprise APIs triggers their self-injection into baseApi
import "@enterprise/lib/store/apis";

// tenantChangeListener invalidates every tenant-scoped RTK Query cache
// when the active tenant changes. Without this, queries keyed by their
// (unchanging) JS arg keep serving the previous tenant's data even
// though the URL the baseQuery would now hit is different. The list
// MUST stay in sync with TENANT_SCOPED_PREFIXES in baseApi.ts.
const tenantChangeListener = createListenerMiddleware();
tenantChangeListener.startListening({
	matcher: isAnyOf(setCurrentTenant, clearTenant),
	effect: (_action, listenerApi) => {
		listenerApi.dispatch(
			baseApi.util.invalidateTags([
				"Providers",
				"ProviderKeys",
				"VirtualKeys",
				"MCPClients",
				"Teams",
				"Customers",
				"Budgets",
				"RateLimits",
				"DBKeys",
				// Logs + dashboard analytics filter by tenant_id query
				// param (Stage 4 backend, Stage 5 UI). Without invalidating
				// these tags, the dashboard would keep serving the previous
				// tenant's histograms until the user manually changed the
				// date range or refreshed the page.
				"Logs",
				"UsageStats",
			]),
		);
	},
});

export const store = configureStore({
	reducer: {
		// RTK Query API
		[baseApi.reducerPath]: baseApi.reducer,
		// App state slice
		app: appReducer,
		// Provider state slice
		provider: providerReducer,
		// Plugin state slice
		plugin: pluginReducer,
		// Tenant state slice (multi-tenant active selection)
		tenant: tenantReducer,
		// Enterprise reducers (if available)
		...enterpriseReducers,
	},
	middleware: (getDefaultMiddleware) =>
		getDefaultMiddleware({
			serializableCheck: {
				// Ignore these action types for RTK Query
				ignoredActions: [
					"persist/PERSIST",
					"persist/REHYDRATE",
					"api/executeQuery/pending",
					"api/executeQuery/fulfilled",
					"api/executeQuery/rejected",
					"api/executeMutation/pending",
					"api/executeMutation/fulfilled",
					"api/executeMutation/rejected",
				],
				// Ignore these field paths in all actions
				ignoredActionsPaths: ["meta.arg", "payload.timestamp"],
				// Ignore these paths in the state
				ignoredPaths: ["api.queries", "api.mutations"],
			},
		})
			// tenant invalidation runs BEFORE the RTK Query middleware so
			// the dispatched invalidateTags lands as a normal action that
			// RTK Query handles in its own slice.
			.prepend(tenantChangeListener.middleware)
			.concat(baseApi.middleware),
	devTools: process.env.NODE_ENV !== "production",
});

export type RootState = ReturnType<typeof store.getState> & EnterpriseState;

export type AppDispatch = typeof store.dispatch;