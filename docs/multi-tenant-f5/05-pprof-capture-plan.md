# Plan — real pprof capture during bench runs

The bench framework today captures the gateway's `/api/dev/pprof` JSON
summary endpoint (`alloc`, `heapInuse`, `numGoroutine`, `numGC`,
top-N in-use allocations). That's enough for "did this scenario add
2000 goroutines?" but **insufficient for flamegraph-driven analysis**
of where the hot path actually spends time. To answer "what call
chain in the dispatcher is eating CPU under mt-skewed load" we need
real `runtime/pprof` profiles.

> **2026-06-17 update — patch15 IS NOT NEEDED.**
>
> While string-mining the OSS `maximhq/bifrost:v1.5.15` binary for the
> SSRF bypass we discovered upstream bifrost already exposes real
> `/debug/pprof/*` endpoints natively, gated behind a dedicated
> listener controlled by environment variables. The patch this doc
> originally proposed (~30 LOC in `transports/bifrost-http/handlers/devpprof.go`)
> is therefore obsolete — the gateway-side capability already exists.
>
> The doc below preserves the original plan for context. **Skip to
> [§ Operational recipe (current path)](#operational-recipe-current-path)
> for the actual workflow.**

## What we need

The standard Go pprof endpoints (registered by `net/http/pprof`):

| Endpoint | Returns | Use |
|---|---|---|
| `GET /debug/pprof/profile?seconds=N` | `.pb.gz` CPU profile sampled for N seconds | `go tool pprof -top`, flamegraphs |
| `GET /debug/pprof/heap` | `.pb.gz` heap profile (inuse + alloc) | Allocation hot spots |
| `GET /debug/pprof/goroutine` | `.pb.gz` goroutine dump (or `?debug=2` for human-readable text) | Deadlock / blocked-goroutine analysis |
| `GET /debug/pprof/block` | `.pb.gz` block profile | Lock contention (must enable via `runtime.SetBlockProfileRate`) |
| `GET /debug/pprof/mutex` | `.pb.gz` mutex contention profile | Lock contention (must enable via `runtime.SetMutexProfileFraction`) |
| `GET /debug/pprof/allocs` | `.pb.gz` alloc profile | Allocation tracking |
| `GET /debug/pprof/threadcreate` | `.pb.gz` thread-create profile | Goroutine leak analysis |

All gated behind admin auth on the f5xc fork, but the gateway already
has `BIFROST_DEV_MODE` (or equivalent) controlling the dev-pprof JSON
endpoint. Same gate.

## Gateway-side change (one small patch)

**File:** `transports/bifrost-http/handlers/devpprof.go` (extend the
existing `DevPprofHandler.RegisterRoutes`).

**Diff (~30 LOC):**

```go
import (
    nethttp "net/http"          // standard library, separate alias
    _ "net/http/pprof"           // side-effect: registers /debug/pprof/* on http.DefaultServeMux
)

func (h *DevPprofHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
    h.collector.Start()

    // Existing JSON summary endpoints.
    r.GET("/api/dev/pprof", lib.ChainMiddlewares(h.getPprof, middlewares...))
    r.GET("/api/dev/pprof/goroutines", lib.ChainMiddlewares(h.getGoroutines, middlewares...))

    // Standard runtime/pprof endpoints, served by net/http/pprof's
    // package-level handlers. Wrap with fasthttp/router's net/http
    // adapter (fasthttpadaptor.NewFastHTTPHandler from
    // github.com/valyala/fasthttp/fasthttpadaptor).
    pprofHandler := fasthttpadaptor.NewFastHTTPHandler(nethttp.DefaultServeMux)
    r.GET("/debug/pprof/{name}", lib.ChainMiddlewares(
        func(ctx *fasthttp.RequestCtx) { pprofHandler(ctx) },
        middlewares...,
    ))
    r.GET("/debug/pprof/", lib.ChainMiddlewares(
        func(ctx *fasthttp.RequestCtx) { pprofHandler(ctx) },
        middlewares...,
    ))

    // Enable block + mutex profiles. The runtime defaults to 0 (off)
    // so without this they always return empty profiles. Cost is
    // ~1% of overhead per non-zero setting; acceptable for the
    // dev/bench use case where this handler is enabled.
    runtime.SetBlockProfileRate(1)
    runtime.SetMutexProfileFraction(1)
}
```

Ship as **patch15** (`mt-debug-pprof-endpoints`). Single file change,
single import addition, ~30 LOC. Keep it inside the existing
`DevPprofHandler` so it's gated by the same `BIFROST_DEV_MODE`
toggle — production deployments stay off by default.

## Bench-side change

**File:** `go-bifrost-ai/examples/bench/profile.go` (extend
alongside the existing `capturePprof` JSON snapshot).

**New flags on the bench:**

```
--profile-cpu DURATION   Sample CPU for DURATION during the run (e.g. 10s).
                         Written as profiles/{scenario}-{timestamp}-cpu.pb.gz.
--profile-mem            Capture /debug/pprof/heap before + after each scenario.
                         Written as {scenario}-{timestamp}-heap-{before,after}.pb.gz.
--profile-block          Capture /debug/pprof/block at end of each scenario.
--profile-mutex          Capture /debug/pprof/mutex at end of each scenario.
--profile-out DIR        Where to write .pb.gz files (default ./profiles).
--profile-tool           If set, run `go tool pprof -top -nodecount=10`
                         on each captured profile and include the top-N
                         in the markdown report.
```

**Hot-path capture timing:**
- CPU profile starts at the beginning of measurement (after warmup) and
  runs for `--profile-cpu` duration. The bench coordinates so the CPU
  sample window overlaps with the steady-state load, not the warmup
  or teardown.
- Heap profiles snap once before + once after — diff shows what grew.
- Block / mutex profiles snap once at end — these are cumulative since
  process start, so the delta isn't as meaningful as the absolute
  hottest contention sites.

**File naming convention:**

```
profiles/
  2026-06-17T05-mt-balanced-cpu.pb.gz
  2026-06-17T05-mt-balanced-heap-before.pb.gz
  2026-06-17T05-mt-balanced-heap-after.pb.gz
  2026-06-17T05-mt-balanced-block.pb.gz
  2026-06-17T05-mt-balanced-mutex.pb.gz
```

**Markdown report extension:**

When `--profile-tool` is set, the report includes per-profile top-10
hot spots:

```
### CPU profile (10s of mt-balanced)

|  Flat % | Cum % | Function |
|---:|---:|---|
| 12.4% | 18.3% | `github.com/maximhq/bifrost/core/(*Bifrost).ChatCompletionRequest` |
| 8.1%  | 9.2%  | `github.com/valyala/fasthttp.(*RequestCtx).PostBody` |
| ...   | ...   | ... |

Full profile: `profiles/2026-06-17T05-mt-balanced-cpu.pb.gz`
View: `go tool pprof -http=:8080 profiles/2026-06-17T05-mt-balanced-cpu.pb.gz`
```

## Concrete capture recipe (post-patch15 + bench update)

```bash
# Run mt-balanced with full profile suite.
BIFROST_URL=http://172.23.255.200 \
BIFROST_ADMIN_USER=admin BIFROST_ADMIN_PASS=... BIFROST_VLLM_KEY=clowntown123 \
go run ./examples/bench --scenario mt-balanced --tenants 4 \
    --use-mock --concurrency 64 --duration 30s \
    --host admin.codeburro2.net \
    --profile-cpu 20s --profile-mem --profile-block --profile-mutex \
    --profile-out /tmp/bench-profiles --profile-tool \
    --report-out /tmp/bench-report.md

# Then explore interactively:
go tool pprof -http=:8080 /tmp/bench-profiles/2026-06-17T05-mt-balanced-cpu.pb.gz
```

## Out of scope for v1

- **Continuous profiling** (Parca / pyroscope / Grafana Phlare). Useful
  in production; overkill for the bench framework.
- **Diff profiling** (capturing one CPU profile per scenario and
  diffing them). `go tool pprof -base` already does this; the bench
  doesn't need to bake in the diff workflow — the operator runs it
  by hand against the saved profiles.
- **Auto-flamegraph rendering**. `go tool pprof -http=` ships its
  own flamegraph viewer; we don't need to bundle one.

## Dependencies + sequencing

1. Land **patch15** (`mt-debug-pprof-endpoints`) on the f5xc-mt-overlay
   branch.
2. Build + deploy a new bifrost image.
3. Extend the bench's `profile.go` with the capture functions + new
   flags.
4. Run a smoke benchmark with `--profile-cpu 10s` and confirm a
   loadable `.pb.gz` lands in the output dir.
5. (Optional) Wire the `--profile-tool` flag's top-N extraction into
   the markdown report.

Estimated total effort: **4-6 hours** end to end. Patch15 is ~30
minutes; bench extensions are ~2 hours; tool integration + report
formatting ~1-2 hours; smoke tests ~30 minutes.

## Why not capture profiles client-side

A naive alternative: don't change the gateway, run the bench under
`go test -cpuprofile`. Doesn't work — the bench is the LOAD
GENERATOR, not the system under test. Profiling the load generator
tells you about its HTTP client, not about the gateway. We need the
profile from the gateway process itself.

---

## Operational recipe (current path)

> Supersedes the patch15 plan above.

OSS bifrost (and therefore both the upstream `maximhq/bifrost:*`
images and the f5xc fork built from the same base) honors these env
vars to spin up a dedicated `runtime/pprof` listener:

| Env var | Purpose |
|---|---|
| `BIFROST_PPROF_PORT` | Listen port for the pprof endpoints. Set to a non-empty value to enable the listener. |
| `BIFROST_PPROF_HOST` | Bind host (default `localhost`; set to `0.0.0.0` to reach from outside the pod). |
| `BIFROST_PPROF_USERNAME` | Basic auth username protecting the endpoints. |
| `BIFROST_PPROF_PASSWORD` | Basic auth password. |
| `BIFROST_PPROF_BLOCK_RATE` | `runtime.SetBlockProfileRate(N)` value. Set to `1` to enable block profiles. |
| `BIFROST_PPROF_MUTEX_FRACTION` | `runtime.SetMutexProfileFraction(N)` value. Set to `1` to enable mutex profiles. |

The standard `/debug/pprof/*` endpoints live on that listener.
Production deployments leave the envs unset → no listener → no
attack surface.

### Wire the envs into the StatefulSet

For ad-hoc benchmarking, patch the existing StatefulSet:

```bash
kubectl -n bifrost-system patch sts/bifrost --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"BIFROST_PPROF_PORT","value":"6060"}},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"BIFROST_PPROF_HOST","value":"0.0.0.0"}},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"BIFROST_PPROF_USERNAME","value":"pprof"}},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"BIFROST_PPROF_PASSWORD","valueFrom":{"secretKeyRef":{"name":"bifrost-pprof","key":"password"}}}},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"BIFROST_PPROF_BLOCK_RATE","value":"1"}},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"BIFROST_PPROF_MUTEX_FRACTION","value":"1"}}
]'
```

For durable deployments these belong in your helm values / kustomize
overlay (`bm-apps/charts/bifrost/values.yaml` for our cluster).

### Capture during a bench run

The pprof listener is on a SECOND port (default `6060`), not the
main HTTP port. Operators reach it either via port-forward or via a
sibling HTTPRoute on the gateway:

```bash
# port-forward path
kubectl -n bifrost-system port-forward sts/bifrost 6060:6060 &
PPROF_USER=pprof PPROF_PASS=...

# CPU profile while load is running (run in a SECOND shell)
curl -sS -u "$PPROF_USER:$PPROF_PASS" \
    "http://localhost:6060/debug/pprof/profile?seconds=20" \
    > profiles/$(date +%Y%m%dT%H%M)-mt-balanced-cpu.pb.gz

# Heap snapshot before/after the bench run
curl -sS -u "$PPROF_USER:$PPROF_PASS" \
    http://localhost:6060/debug/pprof/heap > heap-before.pb.gz
go run ./examples/bench --scenario mt-balanced ...
curl -sS -u "$PPROF_USER:$PPROF_PASS" \
    http://localhost:6060/debug/pprof/heap > heap-after.pb.gz

# Open the flamegraph
go tool pprof -http=:8080 profiles/2026-06-17T*-mt-balanced-cpu.pb.gz
```

### Bench-side automation (optional, ~2h of work)

Wrapping the curl loop into the bench framework so a single command
produces both the markdown report and the `.pb.gz` files. New flags:

```
--profile-cpu DURATION     CPU sample while load runs (e.g. 20s); saved
                           as profiles/{scenario}-{timestamp}-cpu.pb.gz.
--profile-mem              Capture /heap before + after each scenario.
--profile-block            Capture /block at end of each scenario.
--profile-mutex            Capture /mutex at end of each scenario.
--pprof-url URL            Base URL for the dedicated pprof listener
                           (default http://localhost:6060 — port-forward
                           is the assumed path). Can be a sibling
                           HTTPRoute for durable deploys.
--pprof-user / --pprof-pass  Basic auth for the listener.
--profile-out DIR          Where to write .pb.gz files (default ./profiles).
--profile-tool             Run `go tool pprof -top -nodecount=10` on each
                           captured profile; include the top-N in the
                           markdown report.
```

Implementation lives in `go-bifrost-ai/examples/bench/profile.go`
alongside the existing `capturePprof` JSON snapshot. The existing
JSON capture stays — it's cheap and gives the high-level summary;
the new `.pb.gz` capture is for when you want to drill in.

### Security posture

- The dedicated listener is OPT-IN via env vars. Default state: no
  listener, zero new attack surface.
- Basic auth gates the endpoints. Use a strong password from a Secret;
  don't bake into the StatefulSet.
- `BIFROST_PPROF_HOST=0.0.0.0` is required to reach the listener via
  port-forward or service, but ALSO required for accidental network
  exposure if a ClusterIP service is mis-scoped. Default to
  `BIFROST_PPROF_HOST=localhost` for the safest posture — operators
  who need profiles `kubectl exec` into the pod and curl from inside
  rather than exposing the port.
- Production deployments leave all `BIFROST_PPROF_*` envs unset.

### What we lost vs the original patch15 plan

- The original plan put `/debug/pprof/*` on the SAME listener as the
  main HTTP path (gated by the existing admin middleware). The
  current upstream path uses a SECOND port with its own basic auth.
  Slightly more operational ceremony (a port-forward or a sibling
  HTTPRoute) but cleaner security boundary.
- `BIFROST_DEV_MODE` no longer gates `/debug/pprof/*` — it still gates
  the existing JSON `/api/dev/pprof` endpoint. The two are now
  independent toggles.
