package fixtures

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const defaultNodeImage = "kindest/node:v1.31.4"

type KindCluster struct {
	Name       string
	Kubeconfig string // absolute path to a writable kubeconfig for this cluster
	NodeImage  string

	t       *testing.T
	created bool // true if this fixture created the cluster (vs reused)
}

type KindOption func(*kindOptions)

type kindOptions struct {
	name      string
	nodeImage string
	workers   int
	reuse     bool
}

func WithKindName(name string) KindOption     { return func(o *kindOptions) { o.name = name } }
func WithKindNodeImage(img string) KindOption { return func(o *kindOptions) { o.nodeImage = img } }
func WithKindWorkers(n int) KindOption        { return func(o *kindOptions) { o.workers = n } }
func WithKindReuse() KindOption               { return func(o *kindOptions) { o.reuse = true } }

// NewKindCluster ensures a kind cluster exists and returns a handle plus a
// teardown func. If WithKindReuse is set, an existing cluster of the same
// name is reused and the teardown is a no-op.
func NewKindCluster(t *testing.T, opts ...KindOption) (*KindCluster, func()) {
	t.Helper()

	o := &kindOptions{
		name:      "bifrost-test",
		nodeImage: defaultNodeImage,
	}
	for _, opt := range opts {
		opt(o)
	}

	if _, err := exec.LookPath("kind"); err != nil {
		t.Skipf("kind not installed: %v (run `make install-dev-tools`)", err)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skipf("kubectl not installed: %v", err)
	}

	exists, err := kindClusterExists(o.name)
	if err != nil {
		t.Fatalf("kind get clusters: %v", err)
	}

	kc := &KindCluster{
		Name:      o.name,
		NodeImage: o.nodeImage,
		t:         t,
	}

	if exists {
		if !o.reuse {
			t.Fatalf("kind cluster %q already exists; use WithKindReuse() or `kind delete cluster --name %s`", o.name, o.name)
		}
		t.Logf("reusing existing kind cluster %q", o.name)
	} else {
		t.Logf("creating kind cluster %q (node image %s)", o.name, o.nodeImage)
		if err := createKindCluster(o); err != nil {
			t.Fatalf("create kind cluster: %v", err)
		}
		kc.created = true
	}

	kubeconfig, err := exportKubeconfig(o.name)
	if err != nil {
		t.Fatalf("export kubeconfig: %v", err)
	}
	kc.Kubeconfig = kubeconfig

	// kind writes the kubeconfig server as the host-port mapping
	// (127.0.0.1:<port>), which is unreachable from the test process on a
	// shared / docker-in-docker daemon (e.g. CI). Repoint it at the
	// control-plane container's bridge IP on :6443 — routable both locally
	// (Linux) and from a sibling CI container, and covered by the kind API
	// server cert SANs. Best-effort: on failure keep the default kubeconfig.
	if err := repointKubeconfigToContainerIP(o.name, kubeconfig); err != nil {
		t.Logf("repoint kubeconfig to control-plane container IP (using default 127.0.0.1): %v", err)
	}

	if err := kc.waitForNodesReady(2 * time.Minute); err != nil {
		t.Fatalf("nodes ready: %v", err)
	}

	teardown := func() {
		if !kc.created || o.reuse {
			return
		}
		if KeepInfra() {
			t.Logf("keep-infra set, leaving kind cluster %q running", kc.Name)
			return
		}
		t.Logf("deleting kind cluster %q", kc.Name)
		cmd := exec.Command("kind", "delete", "cluster", "--name", kc.Name)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("kind delete (ignored): %v: %s", err, out)
		}
		_ = os.Remove(kc.Kubeconfig)
	}
	return kc, teardown
}

func (k *KindCluster) Kubectl(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k.Kubeconfig)
	return cmd
}

func (k *KindCluster) waitForNodesReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := k.Kubectl(context.Background(), "wait", "--for=condition=Ready", "nodes", "--all", "--timeout=10s")
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		k.t.Logf("waiting for nodes: %v: %s", err, strings.TrimSpace(string(out)))
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for nodes ready after %s", timeout)
}

func kindClusterExists(name string) (bool, error) {
	out, err := exec.Command("kind", "get", "clusters").Output()
	if err != nil {
		return false, fmt.Errorf("kind get clusters: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

func createKindCluster(o *kindOptions) error {
	args := []string{"create", "cluster", "--name", o.name, "--image", o.nodeImage, "--wait", "120s"}

	if o.workers > 0 {
		cfg, err := os.CreateTemp("", "kind-config-*.yaml")
		if err != nil {
			return fmt.Errorf("temp config: %w", err)
		}
		defer os.Remove(cfg.Name())

		fmt.Fprintln(cfg, "kind: Cluster")
		fmt.Fprintln(cfg, "apiVersion: kind.x-k8s.io/v1alpha4")
		fmt.Fprintln(cfg, "nodes:")
		fmt.Fprintln(cfg, "- role: control-plane")
		for i := 0; i < o.workers; i++ {
			fmt.Fprintln(cfg, "- role: worker")
		}
		if err := cfg.Close(); err != nil {
			return fmt.Errorf("close config: %w", err)
		}
		args = append(args, "--config", cfg.Name())
	}

	cmd := exec.Command("kind", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kind create cluster: %w: %s", err, out)
	}
	return nil
}

func exportKubeconfig(name string) (string, error) {
	f, err := os.CreateTemp("", "kubeconfig-"+name+"-*.yaml")
	if err != nil {
		return "", err
	}
	f.Close()
	cmd := exec.Command("kind", "export", "kubeconfig", "--name", name, "--kubeconfig", f.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("export kubeconfig: %w: %s", err, out)
	}
	return f.Name(), nil
}

// kindControlPlaneIP returns the docker bridge IP of the cluster's
// control-plane container (<name>-control-plane).
func kindControlPlaneIP(name string) (string, error) {
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		name+"-control-plane",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s-control-plane: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// repointKubeconfigToContainerIP rewrites the kind kubeconfig's server URL from
// the host-port mapping (127.0.0.1:<port>) to the control-plane container's
// bridge IP on :6443, so the API server is reachable on a shared/DinD docker
// daemon. No-op when the container has no IP.
func repointKubeconfigToContainerIP(name, kubeconfigPath string) error {
	ip, err := kindControlPlaneIP(name)
	if err != nil {
		return err
	}
	if ip == "" {
		return nil // leave the default kubeconfig unchanged
	}
	cmd := exec.Command("kubectl", "--kubeconfig", kubeconfigPath,
		"config", "set-cluster", "kind-"+name, "--server=https://"+ip+":6443")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set-cluster kind-%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
