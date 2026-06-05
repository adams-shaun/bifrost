package fixtures

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

const (
	mockLLMImageRepo = "bifrost-mockllm"
	mockLLMImageTag  = "local-test"
)

// MockLLM is a handle to the in-cluster mock LLM service (deployed by the deps
// umbrella chart). InClusterURL is how bifrost reaches it.
type MockLLM struct {
	Namespace    string
	InClusterURL string

	cluster *KindCluster
	t       *testing.T
}

var (
	mockLLMBuildOnce sync.Once
	mockLLMBuildErr  error
)

// buildMockLLMImage builds tests/k8s/fixtures/mockllm/Dockerfile once. The deps
// chart references it as bifrost-mockllm:local-test with pullPolicy: Never, so
// it must be built and kind-loaded before the chart installs.
func buildMockLLMImage(t *testing.T) (string, error) {
	ref := mockLLMImageRepo + ":" + mockLLMImageTag
	mockLLMBuildOnce.Do(func() {
		// The build pulls a distroless base; skip the (re)build when the image is
		// already present locally and a build was requested to be skipped.
		localExists := exec.Command("docker", "image", "inspect", ref).Run() == nil
		if os.Getenv("BIFROST_K8S_SKIP_BUILD") == "1" && localExists {
			t.Logf("BIFROST_K8S_SKIP_BUILD=1 and %s present locally; skipping mock-llm build", ref)
			return
		}
		root, err := repoRoot()
		if err != nil {
			mockLLMBuildErr = err
			return
		}
		ctxDir := filepath.Join(root, "tests", "k8s", "fixtures", "mockllm")
		t.Logf("building %s from %s", ref, ctxDir)
		cmd := exec.Command("docker", "build", "-t", ref, ctxDir)
		cmd.Stdout = newTestLogWriter(t, "mockllm-build")
		cmd.Stderr = newTestLogWriter(t, "mockllm-build")
		if err := cmd.Run(); err != nil {
			if localExists {
				t.Logf("mock-llm build failed (%v) but %s present locally; using local copy", err, ref)
				return
			}
			mockLLMBuildErr = err
		}
	})
	return ref, mockLLMBuildErr
}
