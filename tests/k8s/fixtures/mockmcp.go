package fixtures

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

const (
	mockMCPImageRepo = "bifrost-mockmcp"
	mockMCPImageTag  = "local-test"
	// MockMCPInClusterURL is the streamable-HTTP MCP endpoint bifrost connects to
	// (mcp-go's StreamableHTTP server serves at /mcp). Short DNS name resolves
	// within the deploy namespace.
	MockMCPInClusterURL = "http://mock-mcp:9000/mcp"
)

var (
	mockMCPBuildOnce sync.Once
	mockMCPBuildErr  error
)

// buildMockMCPImage builds tests/k8s/fixtures/mockmcp/Dockerfile once. The deps
// chart references it as bifrost-mockmcp:local-test with pullPolicy: Never, so
// it must be built and kind-loaded before the chart installs.
func buildMockMCPImage(t *testing.T) (string, error) {
	ref := mockMCPImageRepo + ":" + mockMCPImageTag
	mockMCPBuildOnce.Do(func() {
		localExists := exec.Command("docker", "image", "inspect", ref).Run() == nil
		if os.Getenv("BIFROST_K8S_SKIP_BUILD") == "1" && localExists {
			t.Logf("BIFROST_K8S_SKIP_BUILD=1 and %s present locally; skipping mock-mcp build", ref)
			return
		}
		root, err := repoRoot()
		if err != nil {
			mockMCPBuildErr = err
			return
		}
		ctxDir := filepath.Join(root, "tests", "k8s", "fixtures", "mockmcp")
		t.Logf("building %s from %s", ref, ctxDir)
		cmd := exec.Command("docker", "build", "-t", ref, ctxDir)
		cmd.Stdout = newTestLogWriter(t, "mockmcp-build")
		cmd.Stderr = newTestLogWriter(t, "mockmcp-build")
		if err := cmd.Run(); err != nil {
			if localExists {
				t.Logf("mock-mcp build failed (%v) but %s present locally; using local copy", err, ref)
				return
			}
			mockMCPBuildErr = err
		}
	})
	return ref, mockMCPBuildErr
}
