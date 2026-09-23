/*
Copyright 2026 Politecnico di Torino - NetGroup.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testlib

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RunID generates a unique run identifier for cluster naming.
func RunID() string {
	return fmt.Sprintf("cmp-%s", time.Now().Format("20060102-150405"))
}

// ClusterSpec describes one Kind cluster to create.
type ClusterSpec struct {
	Name       string // full Kind cluster name (includes run-ID prefix)
	Role       string // "central", "consumer-1", "provider-1", etc.
	PodCIDR    string
	SvcCIDR    string
	HasWorker  bool   // whether to add a worker node
	Kubeconfig string // populated after creation
}

// GenerateClusterSpecs builds the list of Kind clusters for a given topology.
func GenerateClusterSpecs(runID string, numConsumers, numProviders int) []ClusterSpec {
	specs := make([]ClusterSpec, 0, 1+numConsumers+numProviders)

	idx := 0
	specs = append(specs, ClusterSpec{
		Name:      fmt.Sprintf("%s-central", runID),
		Role:      "central",
		PodCIDR:   cidr(idx, "pod"),
		SvcCIDR:   cidr(idx, "svc"),
		HasWorker: false,
	})
	idx++

	for i := 1; i <= numConsumers; i++ {
		specs = append(specs, ClusterSpec{
			Name:      fmt.Sprintf("%s-consumer-%d", runID, i),
			Role:      fmt.Sprintf("consumer-%d", i),
			PodCIDR:   cidr(idx, "pod"),
			SvcCIDR:   cidr(idx, "svc"),
			HasWorker: true,
		})
		idx++
	}

	for i := 1; i <= numProviders; i++ {
		specs = append(specs, ClusterSpec{
			Name:      fmt.Sprintf("%s-provider-%d", runID, i),
			Role:      fmt.Sprintf("provider-%d", i),
			PodCIDR:   cidr(idx, "pod"),
			SvcCIDR:   cidr(idx, "svc"),
			HasWorker: true,
		})
		idx++
	}

	return specs
}

// cidr allocates a non-overlapping subnet for cluster index idx. Each /16 has
// only one free octet (10.X.0.0/16), which caps a single-octet counter at
// ~15 clusters starting from base 241 — observed as "invalid CIDR address:
// 10.256.0.0/16" once idx overflows 255. Using /20 blocks instead spends two
// octets (10.X.Y.0/20, Y in 16-wide steps), giving room for thousands of
// clusters while still comfortably covering the 1-2 nodes per Kind cluster
// this harness creates (a /20 is 4096 addresses; Calico's default per-node
// block is /24 = 256 addresses).
func cidr(idx int, kind string) string {
	octet2Step := idx / 16
	octet3 := (idx % 16) * 16
	switch kind {
	case "pod":
		return fmt.Sprintf("10.%d.%d.0/20", 241+octet2Step, octet3)
	case "svc":
		return fmt.Sprintf("10.%d.%d.0/20", 96+octet2Step, octet3)
	default:
		panic("cidr: unknown kind " + kind)
	}
}

// KindCreateCluster creates a single Kind cluster from a spec.
func KindCreateCluster(ctx context.Context, spec *ClusterSpec, kubeconfigDir string) error {
	configFile, err := writeKindConfig(spec)
	if err != nil {
		return fmt.Errorf("write kind config for %s: %w", spec.Name, err)
	}
	defer func() { _ = os.Remove(configFile) }()

	kcPath := filepath.Join(kubeconfigDir, spec.Role+".kubeconfig")

	args := []string{
		"create", "cluster",
		"--name", spec.Name,
		"--config", configFile,
		"--kubeconfig", kcPath,
	}

	log.Printf("[infra] creating Kind cluster %s (pod=%s svc=%s)", spec.Name, spec.PodCIDR, spec.SvcCIDR)
	cmd := exec.CommandContext(ctx, "kind", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kind create cluster %s: %w", spec.Name, err)
	}

	spec.Kubeconfig = kcPath
	log.Printf("[infra] cluster %s ready (kubeconfig=%s)", spec.Name, kcPath)
	return nil
}

func writeKindConfig(spec *ClusterSpec) (string, error) {
	var sb strings.Builder
	sb.WriteString("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnetworking:\n")
	fmt.Fprintf(&sb, "  podSubnet: %q\n", spec.PodCIDR)
	fmt.Fprintf(&sb, "  serviceSubnet: %q\n", spec.SvcCIDR)
	sb.WriteString("nodes:\n- role: control-plane\n")
	if spec.HasWorker {
		sb.WriteString("- role: worker\n")
	}

	f, err := os.CreateTemp("", "kind-config-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		_ = f.Close()
		return "", err
	}
	// Checked: kind reads this file right after, so a failed close would show
	// up as a confusing "invalid cluster config" instead of a write error.
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// KindDeleteCluster deletes a Kind cluster by name.
func KindDeleteCluster(ctx context.Context, name string) error {
	log.Printf("[infra] deleting Kind cluster %s", name)
	cmd := exec.CommandContext(ctx, "kind", "delete", "cluster", "--name", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// KindLoadImages loads Docker images into a Kind cluster.
func KindLoadImages(ctx context.Context, clusterName string, images []string) error {
	if len(images) == 0 {
		return nil
	}
	args := []string{"load", "docker-image", "--name", clusterName}
	args = append(args, images...)
	log.Printf("[infra] loading %d images into %s", len(images), clusterName)
	cmd := exec.CommandContext(ctx, "kind", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// LiqoImages is the full set of container images `liqoctl install` (Liqo
// v1.1.2, matching LIQOCTL_VERSION in deploy/standalone/common.sh) pulls for
// a fresh install, plus the uninstaller image its `uninstall` path uses
// (retry_liqo_install's purge calls `liqoctl uninstall` on a failed
// attempt). Preloading them once and `kind load`-ing them into every
// provider/consumer cluster avoids each of the ~50 concurrent clusters
// independently pulling the same images from ghcr.io / k8s.gcr.io.
var LiqoImages = []string{
	"ghcr.io/liqotech/gateway:v1.1.2",
	"ghcr.io/liqotech/gateway/wireguard:v1.1.2",
	"ghcr.io/liqotech/gateway/geneve:v1.1.2",
	"ghcr.io/liqotech/fabric:v1.1.2",
	"ghcr.io/liqotech/liqo-controller-manager:v1.1.2",
	"ghcr.io/liqotech/webhook:v1.1.2",
	"ghcr.io/liqotech/ipam:v1.1.2",
	"ghcr.io/liqotech/crd-replicator:v1.1.2",
	"ghcr.io/liqotech/metric-agent:v1.1.2",
	"ghcr.io/liqotech/cert-creator:v1.1.2",
	"ghcr.io/liqotech/telemetry:v1.1.2",
	"ghcr.io/liqotech/virtual-kubelet:v1.1.2",
	"ghcr.io/liqotech/proxy:v1.1.2",
	"ghcr.io/liqotech/uninstaller:v1.1.2",
	"k8s.gcr.io/ingress-nginx/kube-webhook-certgen:v1.1.1",
}

// UDPEchoImage is the always-on probe responder deployed on every provider
// cluster (config/agent/provider/udpecho.yaml, the "echo-server" Deployment).
// Preloaded and kind-loaded the same way as LiqoImages: some environments
// have no working DNS/egress to ghcr.io from inside a Kind node's own
// network namespace, and a live pull failure there doesn't fail the
// experiment loudly — it just leaves echo-server in ImagePullBackOff
// forever, so every measured-latency probe against that provider times out
// and silently loses to any candidate that did come up (observed: 0/45
// successful probes across every comparative-latency run so far, root
// cause "dial tcp: lookup ghcr.io ...: i/o timeout" in `kubectl describe
// pod`). Only "latest" is published for this image (confirmed via the
// ghcr.io tags API), so there is no version to pin.
const UDPEchoImage = "ghcr.io/liqotech/udpecho:latest"

// craneVersion pins the crane (go-containerregistry) release ensureCrane
// installs when the tool isn't already on PATH.
const craneVersion = "v0.21.9"

// ensureCrane returns a path to a working `crane` binary, downloading the
// pinned release into a cache dir under os.TempDir if it isn't already on
// PATH. Idempotent: a second call in the same run (or a later run, since the
// cache dir isn't wiped) finds the binary and skips the download.
func ensureCrane(ctx context.Context) (string, error) {
	if p, err := exec.LookPath("crane"); err == nil {
		return p, nil
	}
	toolsDir := filepath.Join(os.TempDir(), "fa-tools")
	cranePath := filepath.Join(toolsDir, "crane")
	if _, err := os.Stat(cranePath); err == nil {
		return cranePath, nil
	}
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return "", fmt.Errorf("create tools dir: %w", err)
	}
	log.Printf("[infra] crane not found — installing %s", craneVersion)
	url := fmt.Sprintf("https://github.com/google/go-containerregistry/releases/download/%s/go-containerregistry_Linux_x86_64.tar.gz", craneVersion)
	sh := fmt.Sprintf("curl -fsSL %q | tar -xz -C %q crane", url, toolsDir)
	cmd := exec.CommandContext(ctx, "sh", "-c", sh)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("install crane: %w", err)
	}
	if err := os.Chmod(cranePath, 0o755); err != nil {
		return "", fmt.Errorf("chmod crane: %w", err)
	}
	return cranePath, nil
}

// DockerPullImages pulls each image for linux/amd64 into the host's local
// Docker image store, so later `kind load docker-image` calls (KindLoadImages)
// never re-download per cluster.
//
// This goes through crane, not `docker pull`, because of a documented kind
// bug (https://github.com/kubernetes-sigs/kind/issues/3845 and others):
// when Docker uses the containerd-snapshotter storage backend, an image
// pulled from a multi-platform manifest list stays associated with that full
// index locally even after `docker pull --platform`, which only limits which
// blobs get downloaded. `kind load docker-image` (`docker save | ctr images
// import --all-platforms`) then tries to import every platform the index
// lists, and fails with "ctr: content digest ... not found" for the
// platforms whose blobs were never fetched. `crane pull --platform` resolves
// the index down to a single concrete manifest before ever touching Docker,
// so the tar loaded into Docker's store has no lingering multi-platform
// index to trip over.
func DockerPullImages(ctx context.Context, images []string) error {
	cranePath, err := ensureCrane(ctx)
	if err != nil {
		return fmt.Errorf("ensure crane: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "fa-image-pull-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	for i, img := range images {
		log.Printf("[infra] pulling %s (linux/amd64, via crane)", img)
		tarPath := filepath.Join(tmpDir, fmt.Sprintf("img-%d.tar", i))
		pull := exec.CommandContext(ctx, cranePath, "pull", "--platform=linux/amd64", img, tarPath)
		pull.Stdout = os.Stdout
		pull.Stderr = os.Stderr
		if err := pull.Run(); err != nil {
			return fmt.Errorf("crane pull %s: %w", img, err)
		}
		load := exec.CommandContext(ctx, "docker", "load", "-i", tarPath)
		load.Stdout = os.Stdout
		load.Stderr = os.Stderr
		if err := load.Run(); err != nil {
			return fmt.Errorf("docker load %s: %w", img, err)
		}
		// Best-effort: frees disk as we go; the deferred RemoveAll gets the rest.
		_ = os.Remove(tarPath)
	}
	return nil
}

// DockerBuild runs `make docker-build` in the repo root. The environment is
// passed through, so DOCKER_BUILD_FLAGS (see the Makefile) reaches the build.
func DockerBuild(ctx context.Context, repoRoot string) error {
	log.Printf("[infra] building Docker images (make docker-build, DOCKER_BUILD_FLAGS=%q)",
		os.Getenv("DOCKER_BUILD_FLAGS"))
	cmd := exec.CommandContext(ctx, "make", "docker-build")
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "IMG_PREFIX=federation-autoscaler", "TAG=latest")
	if err := cmd.Run(); err != nil {
		// The usual cause on a restricted host: the build container cannot resolve
		// DNS, so `go mod download` fails as soon as go.mod changes and the cached
		// module layer no longer applies.
		return fmt.Errorf("%w (if the build output shows a DNS or dial error while downloading "+
			"Go modules, rerun with DOCKER_BUILD_FLAGS=--network=host)", err)
	}
	return nil
}

// ContainerIP returns the Docker container IP of a Kind cluster node.
func ContainerIP(ctx context.Context, containerName string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", containerName)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", containerName, err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("no IP found for container %s", containerName)
	}
	return ip, nil
}

// CheckPrerequisites verifies the required tools are installed.
func CheckPrerequisites() error {
	for _, tool := range []string{"docker", "kind", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool %q not found on $PATH: %w", tool, err)
		}
	}
	cmd := exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker is not running: %w", err)
	}
	return nil
}

// standaloneComponents lists the components built by make docker-build.
var standaloneComponents = []string{"broker", "agent", "grpc-server", "mock-eco", "mock-geo"}

// DefaultStandaloneRegistry is the default REGISTRY in deploy/standalone/common.sh.
const DefaultStandaloneRegistry = "docker.io/kazem26"

// standaloneImageName returns the image name the standalone scripts expect:
// REGISTRY/federation-autoscaler-COMPONENT:TAG.
func standaloneImageName(registry, component, tag string) string {
	return fmt.Sprintf("%s/federation-autoscaler-%s:%s", registry, component, tag)
}

// RetagForStandalone re-tags images from the make docker-build naming convention
// (IMG_PREFIX/component:TAG) to the standalone scripts' convention
// (REGISTRY/federation-autoscaler-component:TAG).
func RetagForStandalone(ctx context.Context, imgPrefix, tag, registry string) error {
	for _, comp := range standaloneComponents {
		src := fmt.Sprintf("%s/%s:%s", imgPrefix, comp, tag)
		dst := standaloneImageName(registry, comp, tag)
		log.Printf("[infra] retagging %s → %s", src, dst)
		cmd := exec.CommandContext(ctx, "docker", "tag", src, dst)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("tag %s -> %s: %w", src, dst, err)
		}
	}
	return nil
}

// CentralImages returns images needed on the central cluster (standalone naming).
func CentralImages(registry, tag string) []string {
	return []string{
		standaloneImageName(registry, "broker", tag),
		standaloneImageName(registry, "mock-eco", tag),
		standaloneImageName(registry, "mock-geo", tag),
	}
}

// AgentImages returns images needed on consumer/provider clusters (standalone naming).
func AgentImages(registry, tag string) []string {
	return []string{
		standaloneImageName(registry, "agent", tag),
	}
}

// ConsumerExtraImages returns additional images for consumer clusters (standalone naming).
func ConsumerExtraImages(registry, tag string) []string {
	return []string{
		standaloneImageName(registry, "grpc-server", tag),
	}
}

// RepoRoot finds the repository root by looking for go.mod.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod in any parent directory")
		}
		dir = parent
	}
}
