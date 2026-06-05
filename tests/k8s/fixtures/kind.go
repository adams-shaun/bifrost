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

	t             *testing.T
	ownKubeconfig bool // true if we exported the kubeconfig (vs got it from $KUBECONFIG)
}

// clusterName is the kind cluster the tests attach to. Its lifecycle (create /
// delete) is owned by CI / the Makefile (make kind-up), not the test code.
func clusterName() string {
	if v := os.Getenv("BIFROST_K8S_CLUSTER"); v != "" {
		return v
	}
	return "bifrost-test"
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

// NewKindCluster ATTACHES to a pre-existing kind cluster and returns a handle
// plus a teardown func. It does NOT create or delete the cluster — that
// lifecycle is owned by CI / the Makefile (`make kind-up`). The kubeconfig is
// taken from $KUBECONFIG when set (CI exports an already-repointed one),
// otherwise it is exported from kind and repointed at the control-plane
// container IP. Teardown only removes a kubeconfig we exported ourselves.
func NewKindCluster(t *testing.T, opts ...KindOption) (*KindCluster, func()) {
	t.Helper()

	o := &kindOptions{name: clusterName(), nodeImage: defaultNodeImage}
	for _, opt := range opts {
		opt(o)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skipf("kubectl not installed: %v", err)
	}

	kc := &KindCluster{Name: o.name, NodeImage: o.nodeImage, t: t}

	// Prefer an externally-provided kubeconfig (CI creates the cluster, repoints
	// the kubeconfig at the control-plane container IP, and exports KUBECONFIG).
	if env := os.Getenv("KUBECONFIG"); env != "" {
		if _, err := os.Stat(env); err == nil {
			kc.Kubeconfig = env
			t.Logf("attaching to cluster %q via $KUBECONFIG=%s", o.name, env)
		}
	}
	if kc.Kubeconfig == "" {
		// Local path: the cluster must already exist (created by `make kind-up`).
		if _, err := exec.LookPath("kind"); err != nil {
			t.Skipf("kind not installed and $KUBECONFIG unset: %v", err)
		}
		exists, err := kindClusterExists(o.name)
		if err != nil {
			t.Fatalf("kind get clusters: %v", err)
		}
		if !exists {
			t.Fatalf("kind cluster %q does not exist — create it first: `make -C tests/k8s kind-up` "+
				"(or `kind create cluster --name %s`). Cluster lifecycle is owned by CI/make, not the test.", o.name, o.name)
		}
		kubeconfig, err := exportKubeconfig(o.name)
		if err != nil {
			t.Fatalf("export kubeconfig: %v", err)
		}
		kc.Kubeconfig = kubeconfig
		kc.ownKubeconfig = true
		if err := repointKubeconfigToContainerIP(o.name, kubeconfig); err != nil {
			t.Logf("repoint kubeconfig to control-plane container IP (using default 127.0.0.1): %v", err)
		}
	}

	if err := kc.waitForNodesReady(2 * time.Minute); err != nil {
		t.Fatalf("control plane not reachable for cluster %q: %v", o.name, err)
	}

	teardown := func() {
		if kc.ownKubeconfig && kc.Kubeconfig != "" {
			_ = os.Remove(kc.Kubeconfig)
		}
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
