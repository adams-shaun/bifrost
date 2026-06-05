package fixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// bifrostNamespace is the fixed namespace the whole stack is deployed into.
	// Fixed (not derived from t.Name) because the serelib-rendered bifrost
	// manifests reach postgres/mock-llm by short in-namespace DNS names.
	bifrostNamespace = "bifrost-apisanity"
	// depsRelease is the helm release for the postgres + mock-llm umbrella chart.
	depsRelease = "bifrost-deps"
	// serelibImage renders the bifrost manifests (serectl render).
	serelibImage = "volterra.azurecr.io/ves.io/serelib:latest"
)

// BifrostInstall is a deployed bifrost + its in-cluster dependencies, with a
// host-side port-forward to the bifrost HTTP API at BaseURL.
type BifrostInstall struct {
	Namespace string
	BaseURL   string
	LocalPort int

	// MockLLM points at the in-cluster mock LLM (deployed by the deps chart).
	MockLLM *MockLLM

	cluster      *KindCluster
	t            *testing.T
	stopForwards []func()
}

// KeepInfra reports whether teardown should be skipped (BIFROST_K8S_KEEP=1),
// leaving the cluster/namespace up for manual inspection.
func KeepInfra() bool { return os.Getenv("BIFROST_K8S_KEEP") == "1" }

// NewBifrostInstall provisions the full stack for the API-sanity test:
//  1. build + kind-load bifrost:local-test and bifrost-mockllm:local-test,
//  2. helm-install the deps umbrella chart (postgres + mock-llm),
//  3. render bifrost via serelib (serectl render) and `kubectl apply` it,
//  4. wait for readiness and open a port-forward to the bifrost Service.
//
// Returns the install handle and a teardown func.
func NewBifrostInstall(t *testing.T, c *KindCluster) (*BifrostInstall, func()) {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skipf("helm not installed: %v", err)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not installed: %v", err)
	}

	bf := &BifrostInstall{Namespace: bifrostNamespace, cluster: c, t: t}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// 0. Images: bifrost (patched, local), mock-llm (local), postgres base.
	bifrostRef := BuildLocalImage(t, defaultImageRepo, defaultImageTag)
	c.LoadImage(t, bifrostRef)
	mockRef, err := buildMockLLMImage(t)
	if err != nil {
		t.Fatalf("build mock-llm: %v", err)
	}
	c.LoadImage(t, mockRef)
	c.LoadExternalImagesParallel(t, []string{"postgres:16-alpine"})

	if err := bf.ensureNamespace(ctx); err != nil {
		t.Fatalf("ensure namespace: %v", err)
	}

	// 1. Dependencies (postgres + mock-llm) via the helm umbrella chart.
	bf.installDeps(ctx)

	// 2. Bifrost via serelib render -> kubectl apply.
	bf.renderAndApplyBifrost(ctx)

	// 3. Readiness + host port-forward.
	if err := bf.WaitReady(ctx, 5*time.Minute); err != nil {
		bf.dumpPodLogs()
		t.Fatalf("wait bifrost ready: %v", err)
	}
	port, stop, err := portForwardService(ctx, c, bf.Namespace, "svc/bifrost", 8080)
	if err != nil {
		t.Fatalf("port-forward bifrost: %v", err)
	}
	bf.LocalPort = port
	bf.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	bf.stopForwards = append(bf.stopForwards, stop)

	if err := bf.waitForHTTPReady(30 * time.Second); err != nil {
		bf.dumpPodLogs()
		bf.teardownPartial()
		t.Fatalf("bifrost /health: %v", err)
	}

	bf.MockLLM = &MockLLM{
		Namespace:    bf.Namespace,
		InClusterURL: "http://mock-llm:8000",
		cluster:      c,
		t:            t,
	}

	teardown := func() {
		bf.dumpPodLogs()
		bf.teardownPartial()
		if KeepInfra() {
			t.Logf("BIFROST_K8S_KEEP=1: leaving namespace %q in cluster %q (kubectl -n %s ...)", bf.Namespace, c.Name, bf.Namespace)
			return
		}
		t.Logf("helm uninstall %s -n %s", depsRelease, bf.Namespace)
		un := exec.Command("helm", "uninstall", depsRelease, "-n", bf.Namespace)
		un.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
		if out, err := un.CombinedOutput(); err != nil {
			t.Logf("helm uninstall (ignored): %v: %s", err, out)
		}
		t.Logf("kubectl delete ns %s", bf.Namespace)
		del := c.Kubectl(context.Background(), "delete", "ns", bf.Namespace,
			"--ignore-not-found", "--wait=true", "--timeout=2m")
		if out, err := del.CombinedOutput(); err != nil {
			t.Logf("delete namespace (ignored): %v: %s", err, out)
		}
	}
	return bf, teardown
}

// installDeps helm-installs the postgres + mock-llm umbrella chart and blocks
// (--wait) until both are Available, so bifrost can connect to postgres on boot.
func (bf *BifrostInstall) installDeps(ctx context.Context) {
	t := bf.t
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	chart := filepath.Join(root, "tests", "k8s", "deploy", "deps")
	args := []string{
		"upgrade", "--install", depsRelease, chart,
		"--namespace", bf.Namespace,
		"--wait", "--timeout", "5m",
	}
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+bf.cluster.Kubeconfig)
	cmd.Stdout = newTestLogWriter(t, "helm deps")
	cmd.Stderr = newTestLogWriter(t, "helm deps")
	t.Logf("helm upgrade --install %s (postgres + mock-llm)", depsRelease)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm install deps: %v", err)
	}
}

// renderAndApplyBifrost renders the bifrost manifests with serelib (serectl
// render) into a temp copy of deploy/bifrost (so the repo tree stays clean),
// then applies the rendered k8s/ dir. kubectl ignores the leftover *.tmpl files.
func (bf *BifrostInstall) renderAndApplyBifrost(ctx context.Context) {
	t := bf.t
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	src := filepath.Join(root, "tests", "k8s", "deploy", "bifrost")
	work := t.TempDir()
	if out, err := exec.Command("cp", "-r", src+"/.", work).CombinedOutput(); err != nil {
		t.Fatalf("copy deploy/bifrost: %v: %s", err, out)
	}

	t.Logf("serelib render bifrost manifests (serectl render)")
	render := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-u", strconv.Itoa(os.Getuid()),
		"-v", work+":"+work, "-w", work,
		serelibImage, "serectl", "render", "-c", "context", "k8s")
	render.Stdout = newTestLogWriter(t, "serelib")
	render.Stderr = newTestLogWriter(t, "serelib")
	if err := render.Run(); err != nil {
		t.Fatalf("serelib render: %v", err)
	}

	apply := bf.cluster.Kubectl(ctx, "apply", "-n", bf.Namespace, "-f", filepath.Join(work, "k8s"))
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply bifrost: %v: %s", err, strings.TrimSpace(string(out)))
	}
	t.Logf("applied serelib-rendered bifrost manifests to namespace %s", bf.Namespace)
}

func (bf *BifrostInstall) ensureNamespace(ctx context.Context) error {
	if err := bf.waitForNamespaceGone(ctx, 2*time.Minute); err != nil {
		return fmt.Errorf("wait for prior namespace to terminate: %w", err)
	}
	create := bf.cluster.Kubectl(ctx, "create", "ns", bf.Namespace, "--dry-run=client", "-o", "yaml")
	out, err := create.Output()
	if err != nil {
		return err
	}
	apply := bf.cluster.Kubectl(ctx, "apply", "-f", "-")
	apply.Stdin = bytes.NewReader(out)
	if applied, err := apply.CombinedOutput(); err != nil {
		return fmt.Errorf("apply ns: %w: %s", err, applied)
	}
	return nil
}

// waitForNamespaceGone returns once the namespace is absent or no longer
// Terminating, so a fresh install doesn't race a prior run's teardown.
func (bf *BifrostInstall) waitForNamespaceGone(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	logged := false
	for {
		phase := bf.cluster.Kubectl(ctx, "get", "ns", bf.Namespace, "-o", "jsonpath={.status.phase}")
		out, err := phase.Output()
		if err != nil {
			return nil // namespace doesn't exist — good
		}
		if strings.TrimSpace(string(out)) != "Terminating" {
			return nil
		}
		if !logged {
			bf.t.Logf("namespace %q is Terminating; waiting before recreating", bf.Namespace)
			logged = true
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %q still Terminating after %s", bf.Namespace, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// WaitReady waits until the bifrost server pod reports Ready.
func (bf *BifrostInstall) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastMsg := ""
	for time.Now().Before(deadline) {
		cmd := bf.cluster.Kubectl(ctx, "wait",
			"-n", bf.Namespace,
			"--for=condition=Ready", "pod",
			"-l", "app.kubernetes.io/name=bifrost,app.kubernetes.io/component=server",
			"--timeout=20s",
		)
		if _, err := cmd.CombinedOutput(); err == nil {
			return nil
		}
		msg := fmt.Sprintf("waiting for bifrost pod to be ready (timeout %s)", timeout)
		if nr := bf.notReadyPods(ctx); len(nr) > 0 {
			msg = fmt.Sprintf("waiting for bifrost pods %v (timeout %s)", nr, timeout)
		}
		if msg != lastMsg {
			bf.t.Log(msg)
			lastMsg = msg
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("bifrost pod not ready after %s", timeout)
}

func (bf *BifrostInstall) notReadyPods(ctx context.Context) []string {
	cmd := bf.cluster.Kubectl(ctx, "get", "pods", "-n", bf.Namespace,
		"-l", "app.kubernetes.io/name=bifrost,app.kubernetes.io/component=server",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"="}{.status.conditions[?(@.type=="Ready")].status}{" "}{end}`,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var notReady []string
	for _, tok := range strings.Fields(string(out)) {
		name, status, ok := strings.Cut(tok, "=")
		if ok && status != "True" {
			notReady = append(notReady, name)
		}
	}
	return notReady
}

func (bf *BifrostInstall) waitForHTTPReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := bf.httpClient().Get(bf.BaseURL + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("bifrost /health not 200 after %s", timeout)
}

func (bf *BifrostInstall) httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// PostJSON POSTs a JSON body to a bifrost API path and returns the response,
// raw body, and any error. Used by ConfigureMockProvider and the sanity test.
func (bf *BifrostInstall) PostJSON(ctx context.Context, path string, body any, headers map[string]string) (*http.Response, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bf.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := bf.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp, out, err
}

func (bf *BifrostInstall) teardownPartial() {
	for _, stop := range bf.stopForwards {
		stop()
	}
	bf.stopForwards = nil
}

// dumpPodLogs tails the bifrost server pod logs to the test log on failure
// (before teardown), so a boot/config error survives cluster cleanup. Set
// BIFROST_K8S_LOG_PODS_ALWAYS=1 to dump on success too.
func (bf *BifrostInstall) dumpPodLogs() {
	if bf.t == nil {
		return
	}
	if !bf.t.Failed() && os.Getenv("BIFROST_K8S_LOG_PODS_ALWAYS") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	get := bf.cluster.Kubectl(ctx, "get", "pods", "-n", bf.Namespace,
		"-l", "app.kubernetes.io/name=bifrost,app.kubernetes.io/component=server",
		"-o", "jsonpath={.items[*].metadata.name}")
	out, err := get.Output()
	if err != nil {
		bf.t.Logf("dumpPodLogs: list pods: %v", err)
		return
	}
	for _, pod := range strings.Fields(string(out)) {
		logs := bf.cluster.Kubectl(ctx, "logs", "-n", bf.Namespace, pod, "--tail=120")
		body, err := logs.CombinedOutput()
		if err != nil {
			bf.t.Logf("dumpPodLogs: logs %s: %v: %s", pod, err, strings.TrimSpace(string(body)))
			continue
		}
		bf.t.Logf("=== bifrost pod %s logs (tail=120) ===\n%s=== end %s ===", pod, body, pod)
	}
}
