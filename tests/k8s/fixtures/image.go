package fixtures

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	defaultImageRepo = "bifrost"
	defaultImageTag  = "local-test"
)

// repoRoot walks up from the package directory to find the root of the bifrost
// repo (identified by the presence of transports/Dockerfile.local, used to
// build the local test image).
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "transports", "Dockerfile.local")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find bifrost repo root from %s", dir)
		}
		dir = parent
	}
}

var (
	buildOnce sync.Map // repo+tag -> *sync.Once
	buildErr  sync.Map // repo+tag -> error
)

// BuildLocalImage builds the Bifrost image from transports/Dockerfile.local.
// Concurrent calls for the same repo:tag dedupe to a single build.
// If the env var BIFROST_K8S_SKIP_BUILD=1 is set, the build is skipped and the
// image is assumed to already exist (useful when CI builds it via make).
func BuildLocalImage(t *testing.T, repo, tag string) string {
	t.Helper()

	if repo == "" {
		repo = defaultImageRepo
	}
	if tag == "" {
		tag = defaultImageTag
	}
	ref := repo + ":" + tag

	if os.Getenv("BIFROST_K8S_SKIP_BUILD") == "1" {
		t.Logf("BIFROST_K8S_SKIP_BUILD=1, assuming %s exists", ref)
		return ref
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not installed: %v", err)
	}

	onceVal, _ := buildOnce.LoadOrStore(ref, &sync.Once{})
	once := onceVal.(*sync.Once)

	once.Do(func() {
		root, err := repoRoot()
		if err != nil {
			buildErr.Store(ref, err)
			return
		}
		dockerfile := filepath.Join(root, "transports", "Dockerfile.local")
		if _, err := os.Stat(dockerfile); err != nil {
			buildErr.Store(ref, fmt.Errorf("dockerfile not found: %w", err))
			return
		}
		// Build from the F5XC-patched tree so the image contains the patched
		// governance features (users, vk-id-header, delta-dump) plus our
		// config-propagation watch and self-signed TLS. Marker-aware: if patches
		// are already applied (e.g. by `make test-k8s-image`), leave them; else
		// apply for the build and restore afterwards. (The `make` path builds with
		// SKIP_BUILD=1 and never reaches here.)
		cleanup, err := applyPatchesForImage(t, root)
		if err != nil {
			buildErr.Store(ref, err)
			return
		}
		defer cleanup()
		t.Logf("building %s from %s (this can take a few minutes on first run)", ref, dockerfile)
		cmd := exec.Command("docker", "build",
			"-f", dockerfile,
			"-t", ref,
			"--build-arg", "VERSION="+tag,
			root,
		)
		cmd.Stdout = newTestLogWriter(t, "docker build")
		cmd.Stderr = newTestLogWriter(t, "docker build")
		if err := cmd.Run(); err != nil {
			buildErr.Store(ref, fmt.Errorf("docker build %s: %w", ref, err))
			return
		}
	})
	if e, ok := buildErr.Load(ref); ok && e != nil {
		t.Fatalf("%v", e)
	}
	return ref
}

// applyPatchesForImage ensures the F5XC patch overlays are applied before the
// image is built, and returns a cleanup that restores the tree. It is
// marker-aware: if patches are already applied (f5xc-patches/.applied present),
// it leaves them in place and the cleanup is a no-op, so it never clobbers a
// state the user (or `make`) set up. Otherwise it runs `make apply-patches` and
// the cleanup runs `make clean-patches`.
func applyPatchesForImage(t *testing.T, root string) (func(), error) {
	noop := func() {}
	if _, err := os.Stat(filepath.Join(root, "f5xc-patches", ".applied")); err == nil {
		return noop, nil // already applied (e.g. by `make test-k8s-image`); leave as-is
	}
	if _, err := exec.LookPath("make"); err != nil {
		// No patch tooling available; build whatever is in the tree. This keeps
		// non-patched local experiments working, at the cost of missing features.
		t.Logf("make not found; building image without applying F5XC patches")
		return noop, nil
	}
	t.Logf("applying F5XC patches for image build (make apply-patches)")
	apply := exec.Command("make", "apply-patches")
	apply.Dir = root
	apply.Stdout = newTestLogWriter(t, "apply-patches")
	apply.Stderr = newTestLogWriter(t, "apply-patches")
	if err := apply.Run(); err != nil {
		return noop, fmt.Errorf("make apply-patches: %w", err)
	}
	return func() {
		clean := exec.Command("make", "clean-patches")
		clean.Dir = root
		clean.Stdout = newTestLogWriter(t, "clean-patches")
		clean.Stderr = newTestLogWriter(t, "clean-patches")
		if err := clean.Run(); err != nil {
			t.Logf("make clean-patches (ignored): %v", err)
		}
	}, nil
}

// LoadImageIntoKind sideloads a local docker image into the kind cluster's
// containerd. Safe to call multiple times — kind dedupes by digest.
func (k *KindCluster) LoadImage(t *testing.T, ref string) {
	t.Helper()
	t.Logf("kind load docker-image %s --name %s", ref, k.Name)
	cmd := exec.Command("kind", "load", "docker-image", ref, "--name", k.Name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kind load %s: %v: %s", ref, err, out)
	}
}

// loadExternalImage pulls a public image and sideloads it into kind, returning
// an error instead of failing the test — so it is safe to call from a goroutine
// (t.Fatalf must only be called from the test's own goroutine).
func (k *KindCluster) loadExternalImage(t *testing.T, ref string) error {
	t.Logf("docker pull %s", ref)
	if out, err := exec.Command("docker", "pull", ref).CombinedOutput(); err != nil {
		// A failed re-pull (transient registry/DNS hiccup) is fine if the image
		// is already present locally — load the local copy instead of failing.
		if inspectErr := exec.Command("docker", "image", "inspect", ref).Run(); inspectErr != nil {
			return fmt.Errorf("docker pull %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
		}
		t.Logf("docker pull %s failed (%s) but image is present locally; using local copy",
			ref, strings.TrimSpace(string(out)))
	}
	t.Logf("kind load docker-image %s --name %s", ref, k.Name)
	if out, err := exec.Command("kind", "load", "docker-image", ref, "--name", k.Name).CombinedOutput(); err != nil {
		return fmt.Errorf("kind load %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LoadExternalImagesParallel pulls + sideloads several public images
// CONCURRENTLY (docker pulls and kind loads overlap; containerd handles
// concurrent imports), then fails the test in the calling goroutine if any
// image errored. Dedupes refs so repeated images aren't pulled twice.
func (k *KindCluster) LoadExternalImagesParallel(t *testing.T, refs []string) {
	t.Helper()
	seen := map[string]bool{}
	var uniq []string
	for _, r := range refs {
		if r != "" && !seen[r] {
			seen[r] = true
			uniq = append(uniq, r)
		}
	}
	errs := make([]error, len(uniq))
	var wg sync.WaitGroup
	for i, ref := range uniq {
		wg.Add(1)
		go func(i int, ref string) {
			defer wg.Done()
			errs[i] = k.loadExternalImage(t, ref)
		}(i, ref)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("load external images (parallel): %v", err)
		}
	}
}

// newTestLogWriter returns an io.Writer that streams to t.Log line by line.
func newTestLogWriter(t *testing.T, prefix string) *testLogWriter {
	return &testLogWriter{t: t, prefix: prefix}
}

type testLogWriter struct {
	t      *testing.T
	prefix string
	buf    strings.Builder
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		s := w.buf.String()
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			break
		}
		w.t.Logf("[%s] %s", w.prefix, s[:idx])
		w.buf.Reset()
		w.buf.WriteString(s[idx+1:])
	}
	return len(p), nil
}
