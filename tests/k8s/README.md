# tests/k8s — KiND-based E2E & Benchmark Suite

End-to-end tests and a reusable benchmark suite that boot a **real KiND cluster**,
build/sideload the local Bifrost image, helm-install Bifrost (SQLite or Postgres)
with replicas behind a Service, deploy an in-cluster mock LLM, drive load, and
assert cross-layer invariants (token conservation, cross-replica config
convergence). The suite also stands up an optional observability stack
(Prometheus + Grafana Alloy + Pyroscope eBPF profiling + Grafana) and can archive
the data for offline review.

It is its own Go module (`github.com/maximhq/bifrost/tests/k8s`) and is **not**
part of the root `go.work`, so always build/test it with `GOWORK=off`. All
scenario tests carry the `//go:build k8s` tag, so a normal `go test ./...` skips
them.

## Prerequisites

```bash
make install-dev-tools   # nix develop .#k8sTools — pins kind, kubectl, helm
```

Docker is expected on the host. `go-bifrost-ai` (the typed admin client used for
seeding) resolves via `GOPRIVATE=gopkg.volterra.us` over SSH.

## Quick start

The `Makefile` here is the easiest entry point — it builds the image, runs the
selected test(s), and tears everything down (unless `KEEP=1`):

```bash
cd tests/k8s
make                          # list all targets
make test                     # ALL tests; teardown on, no observability, defaults
make test-benchmark           # one test
make test-benchmark KEEP=1 OBS=1 PROFILE=1   # keep infra + Prometheus/Grafana + eBPF
make test-benchmark-profiled-keep            # pre-baked flavor
make clean                    # delete the reusable kind cluster
```

Knobs (set on any target): `KEEP`, `OBS`, `PROFILE`, `TLS`, `ARCHIVE=<dir>`,
`SKIP_IMAGE=1`, `RUN=<regex>`, `TIMEOUT`, `COUNT`. They map onto the env vars
documented below.

Raw `go test` equivalent (what the Makefile runs):

```bash
make test-k8s-image      # build bifrost:local-test (cached; ~18s after first build)
GOWORK=off BIFROST_K8S_SKIP_BUILD=1 go test -tags=k8s -count=1 -timeout 40m ./scenarios/...
GOWORK=off BIFROST_K8S_SKIP_BUILD=1 go test -tags=k8s -count=1 -timeout 40m -run TestBenchmark ./scenarios/...
```

`BIFROST_K8S_SKIP_BUILD=1` reuses an already-built `bifrost:local-test` instead
of rebuilding inside the test.

## Package structure

```
fixtures/                      # reusable building blocks (composed by scenarios)
  kind.go          NewKindCluster — create/reuse/teardown a kind cluster
  image.go         docker build (transports/Dockerfile.local) + kind load
  bifrost.go       NewBifrostInstall — helm install, Scale, WaitReady, PodEndpoints,
                   ConfigureMockProvider, InClusterServiceURL, ApplyManifest, teardown
  mockllm.go       In-cluster mock OpenAI server (Deployment+Service) + /__stats
  postgres.go      Governance-store / budget readers over a PG port-forward
  portforward.go   Service/pod port-forward helpers
  metrics.go       Per-pod /metrics scrape (token counts)
  ledger.go        Cross-layer token reconciliation (client / pods / mock / PG)
  load.go          Native Go load client (multi-VK, graceful drain) — fallback driver
  governance.go    VK/team/customer create/update/delete + ?from_memory probes
  seed.go          Bulk seed + config-churn via the typed go-bifrost-ai client
  audit.go         AuditLedger + cross-replica convergence audit + propagation report
  bench.go         BenchConfig + DeployParams + KeepInfra/Obs env helpers
  bench_k6.go      In-cluster k6 runner → Service ClusterIP (prometheus-rw + summary)
  bench_obs.go     Observability stack: Prometheus, Alloy(OTLP), Pyroscope(eBPF), Grafana
  bench_archive.go Artifact capture (k6 summary, ledger, pod logs, Prom TSDB snapshot)
  bench_run.go     RunBenchmark — the orchestrator
  scripts/         k6 script (bifrost_chat.js)
  dashboards/      Provisioned Grafana dashboards (k6, bifrost, profiles)
  manifests/       Embedded YAML (mock LLM)
  mockllm/         Source for the mock LLM image
scenarios/         # //go:build k8s tests
  scale_test.go             TestScaleUnderLoad
  quota_test.go             TestQuotaConvergesAcrossReplicas
  propagation_test.go       TestPropagation*
  propagation_matrix_test.go TestPropagationMatrix
  scale_matrix_test.go      TestScaleMatrix
  lww_test.go               (last-writer-wins repro)
  stress_conservation_test.go TestStressConfigSyncConservation
  benchmark_test.go         TestBenchmark (the benchmark suite entry point)
```

## How it fits together

`NewKindCluster` (reused across runs) → `NewBifrostInstall` (helm install at 1
replica, mock LLM, Postgres) → seed config → `Scale(N)` so fresh pods converge
from Postgres → drive load → collect a **token ledger** and a **convergence
audit** → conditional teardown. Config writes go through the Service (a random
replica) and must propagate to peers (PG `LISTEN/NOTIFY` watch + reconcile); the
ledger proves `client == Σ(pods) == mock-LLM`, and the audit proves every
replica's in-memory state matches what was written.

## Current tests

| Test | What it validates | Key tunables (env) |
|---|---|---|
| `TestScaleUnderLoad` | Traffic stays correct while scaling 2→5→3 mid-load; ledger reconciles | — |
| `TestQuotaConvergesAcrossReplicas` | Budget/quota counters converge across replicas | — |
| `TestPropagationMatrix` | Per-entity create/update/delete propagation latency table (VK/team/customer/routing/model-config) | — |
| `TestScaleMatrix` | Propagation across a sweep of replica counts | — |
| `TestStressConfigSyncConservation` | 3 replicas, hundreds of objects/table, multi-VK load via per-pod fan-out, live config churn, propagation report, **convergence audit + exact token conservation** | `BIFROST_STRESS_OBJECTS` (200), `BIFROST_STRESS_CLIENTS` (50), `BIFROST_STRESS_RPS` (2500), `BIFROST_STRESS_DURATION` (2m) |
| `TestBenchmark` | The reusable benchmark suite: sized deploy, seed, **in-cluster k6** vs the Service ClusterIP, optional obs/profiling, churn, then conservation + audit gates | `BIFROST_BENCH_OBJECTS` (100), `BIFROST_BENCH_REPLICAS` (3), `BIFROST_BENCH_RPS` (500), `BIFROST_BENCH_DURATION` (2m), `BIFROST_BENCH_TLS` (off) |
| `TestMultiTenantSanity` | Boots Bifrost with `BIFROST_MULTI_TENANT_ENABLED=true`, provisions two independent tenants, drives one inference per tenant, deletes tenant A's VK and verifies same-replica resolver-cache invalidation (patch 0028) | — |

## Multi-tenant scenarios

`TestMultiTenantSanity` is the first scenario in the F5XC multi-tenant E2E
series (XC-25496). It opts into the new admin surface by passing
`fixtures.WithMultiTenant("")` to `NewBifrostInstall`; the option splices
`BIFROST_MULTI_TENANT_ENABLED=true` into the rendered Deployment before
`kubectl apply` (so the first pod already carries the right config — no
post-apply rollout race). The new `fixtures.MTAdmin` raw-HTTP client
drives `/api/platform/tenants` + `/api/tenants/{tid}/{providers,
governance/virtual-keys}` since the go-bifrost-ai SDK does not yet expose
tenant-scoped methods.

Follow-on scenarios (streaming, async, MCP, multi-replica VK
invalidation, Anthropic-shape isolation) layer onto the same `MTAdmin`
helper.

## Cross-cutting capabilities (apply to EVERY test here)

These are wired into the shared fixtures, so they work for any scenario — set the
env var, no code change needed.

### 1. Keep infrastructure up for review — `BIFROST_K8S_KEEP=1`

Skips **all** teardown: the kind cluster, the helm release, the namespace, and the
observability stack are left running after the test. Honored by both the
`KindCluster` and `BifrostInstall` teardown closures. (Aliases:
`BIFROST_K8S_KEEP_RELEASE`, `BIFROST_BENCH_KEEP`.)

```bash
BIFROST_K8S_KEEP=1 BIFROST_K8S_OBS=1 ... go test -tags=k8s -run TestBenchmark ./scenarios/...
# When done reviewing, clean up:
kubectl delete ns bifrost-testbenchmark --wait=false      # namespace is bifrost-<lowercased test name>
make test-k8s-clean                                       # or nuke the whole kind cluster
```

> Re-running a kept test collides ("already exists") because the namespace
> persists — delete it first.

### 2. Observability stack — `BIFROST_K8S_OBS=1`

Stands up, in the test namespace: **Prometheus** (scrapes each bifrost pod's
`/metrics` + receives k6 remote-write; admin API enabled for snapshots),
**Grafana Alloy** (OTLP collector → Prometheus remote-write), and **Grafana**
(anonymous admin) with provisioned datasources and dashboards (`bench-k6`,
`bench-bifrost`, and — with profiling — `bench-profiles`).

Any test gets it by calling the one-liner (already in `TestStressConfigSyncConservation`
and `TestBenchmark`):

```go
obs := bf.MaybeInstallObservability(t)   // nil unless BIFROST_K8S_OBS / _PROFILE is set
```

Cadence tunables:

| Env | Controls | Default |
|---|---|---|
| `BIFROST_K8S_SCRAPE_INTERVAL` | Prometheus scrape interval (metrics resolution) | `5s` |
| `BIFROST_K8S_PROFILE_RATE` | eBPF CPU samples/sec per process (`pyroscope.ebpf` `sample_rate`) | `97` |
| `BIFROST_K8S_PROFILE_INTERVAL` | eBPF collect+push interval (`pyroscope.ebpf` `collect_interval`) | `15s` |

(The `bench-alloy` OTLP collector receives and remote-writes — it does not scrape;
Bifrost's metrics come via the Prometheus scrape above. Its remote-write flush is
at Alloy's default.)

Browse it (with `BIFROST_K8S_KEEP=1`):

```bash
kubectl port-forward -n <ns> svc/bench-grafana 3000:3000      # http://localhost:3000
kubectl port-forward -n <ns> svc/bench-prometheus 9090:9090
```

### 3. eBPF CPU profiling — `BIFROST_K8S_PROFILE=1` (implies OBS)

Adds **Pyroscope** plus a privileged, `hostPID` **Grafana Alloy DaemonSet** that
continuously CPU-profiles the bifrost pod processes **via kernel eBPF** — no app
pprof endpoint required. Profiles (`process_cpu`, `service_name="bifrost"`) land
in Pyroscope and are viewable in Grafana (the `bench-profiles` flamegraph
dashboard, or Explore → Pyroscope). Profiling is best-effort: if the host kernel
can't support eBPF, the run logs a warning and continues.

```bash
kubectl port-forward -n <ns> svc/bench-pyroscope 4040:4040
```

### 4. Artifact archive — `BIFROST_K8S_ARCHIVE_DIR=/path`

Writes a self-contained run directory: the k6 summary, the token ledger, the
convergence audit, per-pod logs, and a **Prometheus TSDB snapshot** for offline
analysis. (Alias: `BIFROST_BENCH_ARCHIVE_DIR`.)

### 5. Self-signed TLS — `BIFROST_BENCH_TLS=1` (or `DeployParams.TLS`)

Bifrost serves **HTTPS** instead of plaintext. Bifrost generates an in-memory
ECDSA self-signed cert at boot (`BIFROST_TLS_SELF_SIGNED`, handled natively by
the fasthttp server via `ServeTLSEmbed` — note fasthttp is HTTP/1.1 only, no
HTTP/2). The whole harness follows: `BaseURL`/`InClusterServiceURL`/pod
endpoints switch to `https://`, every harness client (PostJSON, governance/audit
probes, `/metrics` scrape, the load generator, the typed admin client) and k6
skip cert verification, and the kubelet liveness/readiness probes are set to the
`HTTPS` scheme via helm. (For a real cert instead, set `BIFROST_TLS_CERT_FILE` +
`BIFROST_TLS_KEY_FILE` on the deployment.)

## Writing a new benchmark

Compose `BenchConfig` and call `fixtures.RunBenchmark(t, cfg)`; assertions are
left to you so the same orchestration backs both pass/fail tests and exploratory
runs:

```go
res := fixtures.RunBenchmark(t, fixtures.BenchConfig{
    Name:     "my-bench",
    Seed:     fixtures.SeedSpec{VirtualKeys: 100, Teams: 50, Customers: 50},
    Deploy:   fixtures.DeployParams{Replicas: 5, Storage: fixtures.StoragePostgres,
                                    Resources: fixtures.ResourceParams{RequestsCPU: "500m", LimitsCPU: "2"}},
    Workload: fixtures.K6Workload{Rate: 1000, Duration: 5 * time.Minute, Models: []string{"openai/gpt-4o"}},
    Obs:      fixtures.ObsParamsFromEnv(), // env-driven obs/profiling
    Churn:    true,
})
if res.Ledger != nil { _ = res.Ledger.Reconcile(fixtures.DefaultTolerance()) }
```

Or build config arbitrarily with `SeedFn func(t, *BifrostInstall)` (providers,
keys, MCP clients, plugins via the typed client) instead of `Seed`.

## Full example: a kept, observed, profiled benchmark

```bash
make test-k8s-image
cd tests/k8s
BIFROST_K8S_SKIP_BUILD=1 \
BIFROST_K8S_OBS=1 BIFROST_K8S_PROFILE=1 BIFROST_K8S_KEEP=1 \
BIFROST_K8S_ARCHIVE_DIR=$HOME/bifrost-bench \
BIFROST_BENCH_REPLICAS=3 BIFROST_BENCH_RPS=500 BIFROST_BENCH_DURATION=2m \
GOWORK=off go test -tags=k8s -count=1 -timeout 40m -v -run TestBenchmark ./scenarios/...
# then port-forward grafana/pyroscope (printed at the end), and `make test-k8s-clean` when done.
```
