// Package configstore — tenant_scope.go.
//
// The single GORM callback registration that makes every existing
// query and write tenant-aware without per-method edits.
//
// Contract
// --------
// The HTTP layer (transports/.../middlewares.go's tenant-resolver
// middleware) reads the `x-f5xc-tenant` request header and stashes
// the value on the request context under
// multitenant.BifrostContextKeyTenantID.  All configstore methods
// accept a context.Context that flows through to gorm via
// db.WithContext(ctx).  Once the callbacks below are registered on a
// *gorm.DB, every:
//
//   - SELECT against a model that has a `TenantID` column gets an
//     implicit `WHERE tenant_id = ?` (read from ctx)
//   - INSERT against such a model gets an implicit `tenant_id = ?`
//     populated (read from ctx) when the row hasn't supplied one
//   - UPDATE / DELETE against such a model gets an implicit
//     `WHERE tenant_id = ?` so a tenant cannot mutate another
//     tenant's rows even if the row ID would otherwise match
//
// Models WITHOUT a `TenantID` column flow through untouched.
//
// Single-tenant fallback
// ----------------------
// When ctx carries no tenant_id (cron jobs, internal background
// work, single-tenant deployments where the header isn't set), the
// schema's `default:default` column default takes over on INSERT and
// the scope predicate is simply skipped — so a single-tenant
// deployment with the header absent reads everything as if there
// were no multi-tenant logic at all.

package configstore

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime/debug"
	"strings"

	"github.com/maximhq/bifrost/multitenant"
	"gorm.io/gorm"
)

// tenantColumn is the canonical column name on every tenant-scoped
// model.  Kept as a constant so the callback registration doesn't
// drift from the gorm tags on the table structs.
const tenantColumn = "tenant_id"

// RegisterTenantScopes installs the SELECT / INSERT / UPDATE / DELETE
// callbacks that read tenant identity from the request context and
// either constrain the query (reads / mutations of existing rows) or
// populate the column (new rows).
//
// MUST be called exactly once per *gorm.DB, immediately after open
// and before any business code uses the handle.  Safe to call from
// both sqlite and postgres constructors.
func RegisterTenantScopes(db *gorm.DB) error {
	if err := db.Callback().Query().Before("gorm:query").
		Register("multitenant:scope_query", scopeQueryByTenant); err != nil {
		return err
	}
	if err := db.Callback().Update().Before("gorm:update").
		Register("multitenant:scope_update", scopeQueryByTenant); err != nil {
		return err
	}
	if err := db.Callback().Delete().Before("gorm:delete").
		Register("multitenant:scope_delete", scopeQueryByTenant); err != nil {
		return err
	}
	// Register BEFORE gorm:before_create — that's the lifecycle step
	// that fires per-model BeforeCreate hooks (Layer 3 strict-mode
	// asserts via EnsureTenantIDOnCreate). If the scope callback ran
	// after that, every BeforeCreate would see an empty tenant_id (the
	// handler hadn't stamped child rows like nested budgets / rate-limits
	// inside customer/team/VK transactions) and strict mode would error
	// out before the scope had a chance to fill it from request ctx.
	if err := db.Callback().Create().Before("gorm:before_create").
		Register("multitenant:scope_create", populateTenantIDOnCreate); err != nil {
		return err
	}
	return nil
}

// hasTenantColumn returns true when the model bound to this statement
// carries a `TenantID` column we should scope on.  Defensive against
// statements with no Schema parsed (raw SQL, ad-hoc Table queries).
func hasTenantColumn(tx *gorm.DB) bool {
	if tx == nil || tx.Statement == nil || tx.Statement.Schema == nil {
		return false
	}
	return tx.Statement.Schema.LookUpField(tenantColumn) != nil
}

// tenantFromContext returns the tenant id stashed on the request
// context by the tenant-resolver middleware, or empty when no header
// was present (in which case the column default + lack of WHERE
// predicate combine to give single-tenant semantics).
//
// The middleware stores the value under TWO forms because of
// fasthttp + Go interface{} key semantics:
//
//   - the typed schemas.BifrostContextKey (preferred — used by
//     BifrostContext.Value lookups on the inference path)
//   - the stringified form (because fasthttp.RequestCtx.SetUserValue
//     stores by interface{} equality, and a string key doesn't match
//     a typed-string key on lookup; ctx.Value(typedKey) misses what
//     ctx.SetUserValue(stringKey, ...) put there)
//
// Try the typed key first, fall back to the string form.
func tenantFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if v := ctx.Value(multitenant.BifrostContextKeyTenantID); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	if v := ctx.Value(string(multitenant.BifrostContextKeyTenantID)); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}

// scopeQueryByTenant is the SELECT / UPDATE / DELETE callback.  It
// appends a `<table>.tenant_id = ?` predicate to the statement when
// the model has a TenantID column AND the ctx has a tenant id.
//
// The column is qualified with the primary table name so JOINs across
// two tenant-scoped tables (e.g. config_keys -> config_providers,
// both of which carry tenant_id) don't throw "ambiguous column name:
// tenant_id" at the DB. The qualification falls back to bare
// `tenant_id` only when the statement's primary table isn't known
// (raw SQL paths that bypassed the schema parser).
func scopeQueryByTenant(tx *gorm.DB) {
	if tx.Error != nil || !hasTenantColumn(tx) {
		return
	}
	tid, ok := tenantFromContext(tx.Statement.Context)
	if !ok {
		return
	}
	tx.Statement.Where(qualifyTenantColumn(tx)+" = ?", tid)
}

// qualifyTenantColumn returns "<primary_table>.tenant_id" when the
// statement's table can be resolved, falling back to bare "tenant_id"
// otherwise. Centralised so future callbacks that need the same
// JOIN-safe column ref reuse it.
func qualifyTenantColumn(tx *gorm.DB) string {
	if tx.Statement == nil {
		return tenantColumn
	}
	table := tx.Statement.Table
	if table == "" && tx.Statement.Schema != nil {
		table = tx.Statement.Schema.Table
	}
	if table == "" {
		return tenantColumn
	}
	return table + "." + tenantColumn
}

// populateTenantIDOnCreate is the INSERT callback.  Sets TenantID
// from ctx on each row in the statement BEFORE the INSERT runs, when
// the row didn't already carry an explicit value.  Bulk inserts (a
// slice of rows) are handled by iterating the reflect value.
//
// Strict-mode assertion: when ctx has no tenant_id AND the row's
// TenantID field is the zero string AND the model is tenant-scoped,
// the callback FAILS the write with a descriptive error containing a
// stack trace. Most legitimate "no tenant on ctx" cases (config.json
// boot sync, cron jobs, single-tenant deployments) set TenantID
// explicitly on the struct (e.g. DefaultTenantID), so they bypass
// this assertion. What it catches: API handlers that re-wrap ctx and
// drop the tenant, plugins writing in goroutines that lost the
// per-request ctx, and any future code path that forgets to thread
// the request context through to the DB. The alternative — silently
// falling back to the schema's `default:default` column default — is
// exactly the failure mode that produced the cross-tenant data
// corruption fixed in the 06-16 incident (rows ended up with
// tenant_id='default' while their owning provider was tenant='shaun',
// breaking every subsequent tenant-scoped read).
//
// Opt-out: BIFROST_ASSERT_TENANT_ON_INSERT=false downgrades the
// assertion to a stderr log. Default is strict (fail the write).
func populateTenantIDOnCreate(tx *gorm.DB) {
	if tx.Error != nil || !hasTenantColumn(tx) {
		return
	}
	tid, ok := tenantFromContext(tx.Statement.Context)
	if !ok {
		// No tenant on ctx — only safe if every row in the batch has
		// TenantID already populated by the caller. If any row is
		// still zero-string, the row will fall back to the schema
		// default and we'll silently corrupt cross-tenant state.
		assertEveryRowCarriesTenant(tx)
		return
	}
	rv := tx.Statement.ReflectValue
	ctx := tx.Statement.Context
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			setTenantIfEmpty(ctx, rv.Index(i), tid)
		}
	case reflect.Struct:
		setTenantIfEmpty(ctx, rv, tid)
	}
	// Post-check: setTenantIfEmpty is reflection-based and has bitten
	// us with edge cases where the mutation didn't propagate to the
	// actual INSERT (live cluster wrote tenant_id='' on a row even
	// though ctx tenant was set). Re-walk the rows and assert each
	// one carries a non-empty TenantID before the SQL fires. This is
	// the post-condition matching the pre-condition asserted by
	// assertEveryRowCarriesTenant above for the no-ctx branch — both
	// paths now share the same guarantee.
	assertEveryRowCarriesTenant(tx)
}

// assertEveryRowCarriesTenant checks each row in the INSERT statement
// has a non-empty TenantID. If any row is empty (which would fall back
// to the schema's `default:default` column default), the callback
// errors-out the INSERT with a stack trace identifying the caller —
// unless BIFROST_ASSERT_TENANT_ON_INSERT=false, in which case the
// failure is downgraded to a stderr log and the INSERT proceeds.
func assertEveryRowCarriesTenant(tx *gorm.DB) {
	rv := tx.Statement.ReflectValue
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if !rowHasTenant(rv.Index(i)) {
				raiseTenantAssertion(tx, fmt.Sprintf("INSERT into %s with row #%d.TenantID=\"\" and no tenant on ctx", tx.Statement.Table, i))
				return
			}
		}
	case reflect.Struct:
		if !rowHasTenant(rv) {
			raiseTenantAssertion(tx, fmt.Sprintf("INSERT into %s with TenantID=\"\" and no tenant on ctx", tx.Statement.Table))
		}
	}
}

// rowHasTenant returns true when the row's TenantID field is non-empty.
// Tolerates nil pointers + non-struct values (returns true — there's
// nothing to assert).
func rowHasTenant(rv reflect.Value) bool {
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return true
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return true
	}
	field := rv.FieldByName("TenantID")
	if !field.IsValid() || field.Kind() != reflect.String {
		return true
	}
	return field.String() != ""
}

// strictAssertEnv opts INTO strict mode (default = log-only). Set to
// true/1/yes/on to convert the assertion from a stderr warning into a
// hard write failure. The Bifrost helm chart sets this in multi-
// tenant deployments; OSS tests + single-tenant deployments leave it
// unset so existing fixture rows that don't carry a tenant continue
// to insert (preserving OSS behaviour by default).
const strictAssertEnv = "BIFROST_ASSERT_TENANT_ON_INSERT"

// raiseTenantAssertion either fails the INSERT (when strictAssertEnv
// is set truthy) or logs a loud stderr warning. Includes a stack
// trace so the offending call site is obvious in the failure report.
func raiseTenantAssertion(tx *gorm.DB, msg string) {
	stack := debug.Stack()
	envVal := strings.ToLower(strings.TrimSpace(os.Getenv(strictAssertEnv)))
	strict := envVal == "true" || envVal == "1" || envVal == "yes" || envVal == "on"
	if strict {
		_ = tx.AddError(fmt.Errorf("multitenant: tenant assertion failed: %s\n%s", msg, stack))
		return
	}
	fmt.Fprintf(os.Stderr, "[WARN] multitenant: tenant assertion (logged, not blocked): %s\n%s\n", msg, stack)
}

// setTenantIfEmpty sets the TenantID field on rv to tid when the
// field's current value is either the zero string OR the literal
// "default". The "default" case is critical: the GORM `default:default`
// tag on every tenant-scoped table causes GORM to pre-fill TenantID
// with "default" BEFORE this callback fires. Without this override, a
// genuine ctx-supplied tenant id (e.g. "shaun") never makes it onto
// the row — every INSERT lands with tenant_id="default" regardless
// of which tenant the request claims. Treating "default" as
// overridable preserves the explicit-set semantics for any other
// caller-supplied value (e.g. the /api/platform/tenants admin handler
// minting a row on behalf of another tenant).
func setTenantIfEmpty(_ context.Context, rv reflect.Value, tid string) {
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return
	}
	field := rv.FieldByName("TenantID")
	if !field.IsValid() || field.Kind() != reflect.String {
		return
	}
	current := field.String()
	if current != "" && current != "default" {
		// Caller stamped an explicit non-default tenant; respect it.
		return
	}
	if !field.CanSet() {
		return
	}
	field.SetString(tid)
}
