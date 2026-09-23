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
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DeployOpts holds everything needed to deploy the full topology.
type DeployOpts struct {
	RepoRoot        string
	RunID           string
	Specs           []ClusterSpec
	KubeconfigDir   string
	CADir           string // temp dir for PKI material
	NumConsumers    int
	NumProviders    int
	ProviderRegions []string
	ImgPrefix       string
	ImgTag          string
	LiqoProvider    string // "kind", "k3s", "kubeadm"

	// EcoCacheTTL overrides the provider's carbon-intensity cache TTL (Go
	// duration, e.g. "5s"). comparative-eco sets this well below its
	// carbonRefreshInterval so deployed providers actually observe the
	// harness's periodic mock-eco re-randomization within a phase, instead
	// of serving their first-fetched value for the agent's 1h default --
	// and so the observation lag (this TTL plus the provider's 30s
	// advertisement cycle) stays a small fraction of one refresh tick.
	// Zero ⇒ leave the agent's default untouched.
	EcoCacheTTL time.Duration
}

// DeployAll deploys the full federation topology onto the Kind clusters.
func DeployAll(ctx context.Context, opts DeployOpts) error {
	standaloneDir := filepath.Join(opts.RepoRoot, "deploy", "standalone")

	centralSpec := opts.Specs[0]
	centralContainerName := centralSpec.Name + "-control-plane"
	centralIP, err := ContainerIP(ctx, centralContainerName)
	if err != nil {
		return fmt.Errorf("get central IP: %w", err)
	}
	log.Printf("[deploy] central cluster IP: %s", centralIP)

	// --- Deploy broker on central ---
	if err := retryDeploy(ctx, "deploy central", func() error {
		return deployCentral(ctx, standaloneDir, centralSpec, centralIP, opts)
	}); err != nil {
		return fmt.Errorf("deploy central: %w", err)
	}

	// --- Deploy mocks on central ---
	if err := retryDeploy(ctx, "deploy mocks", func() error {
		return deployMocks(ctx, standaloneDir, centralSpec, opts)
	}); err != nil {
		return fmt.Errorf("deploy mocks: %w", err)
	}

	// Replace static mocks with controllable versions for tests.
	if err := deployControllableMockEco(ctx, opts.RepoRoot, centralSpec); err != nil {
		return fmt.Errorf("deploy controllable mock-eco: %w", err)
	}
	if err := deployControllableMockGeo(ctx, opts.RepoRoot, centralSpec); err != nil {
		return fmt.Errorf("deploy controllable mock-geo: %w", err)
	}

	mockEcoURL := fmt.Sprintf("http://%s:30081", centralIP)
	mockGeoURL := fmt.Sprintf("http://%s:30080", centralIP)

	// --- Register geo overrides BEFORE deploying agents ---
	// Kind clusters are already created, so Docker bridge IPs are known. Register
	// them now so agents get correct regions on their very first geo lookup.
	if err := registerGeoOverrides(ctx, mockGeoURL, opts); err != nil {
		return fmt.Errorf("register geo overrides: %w", err)
	}

	// --- Mint join bundles for all consumers and providers ---
	bundleDir := filepath.Join(opts.CADir, "bundles")
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		return fmt.Errorf("create bundle dir: %w", err)
	}

	clusterIDs := allClusterIDs(opts)
	for _, cid := range clusterIDs {
		if err := mintJoinBundle(ctx, standaloneDir, opts.CADir, cid, bundleDir); err != nil {
			return fmt.Errorf("mint bundle for %s: %w", cid, err)
		}
	}

	// --- Deploy consumers ---
	for i := 0; i < opts.NumConsumers; i++ {
		spec := opts.Specs[1+i]
		cid := fmt.Sprintf("consumer-%d", i+1)
		bundlePath := filepath.Join(bundleDir, cid+"-bundle.tgz")
		containerName := spec.Name + "-control-plane"
		consumerIP, err := ContainerIP(ctx, containerName)
		if err != nil {
			return fmt.Errorf("get consumer-%d IP: %w", i+1, err)
		}
		if err := retryDeploy(ctx, fmt.Sprintf("deploy consumer-%d", i+1), func() error {
			return deployConsumer(ctx, standaloneDir, spec, cid, bundlePath, consumerIP, mockGeoURL, opts)
		}); err != nil {
			return fmt.Errorf("deploy consumer-%d: %w", i+1, err)
		}
	}

	// --- Deploy providers ---
	for i := 0; i < opts.NumProviders; i++ {
		spec := opts.Specs[1+opts.NumConsumers+i]
		cid := fmt.Sprintf("provider-%d", i+1)
		bundlePath := filepath.Join(bundleDir, cid+"-bundle.tgz")
		if err := retryDeploy(ctx, fmt.Sprintf("deploy provider-%d", i+1), func() error {
			return deployProvider(ctx, standaloneDir, spec, cid, bundlePath, mockEcoURL, mockGeoURL, opts)
		}); err != nil {
			return fmt.Errorf("deploy provider-%d: %w", i+1, err)
		}
	}

	return nil
}

// liqoProviderKind is the InfraConfig.LiqoProvider value these tests run on:
// the deploy scripts pass provider-specific flags to liqoctl, and only the
// Kind path is exercised here.
const liqoProviderKind = "kind"

// deployMaxAttempts / deployRetryDelay bound retryDeploy below.
const deployMaxAttempts = 3
const deployRetryDelay = 15 * time.Second

// retryDeploy runs fn up to deployMaxAttempts times, sleeping deployRetryDelay
// between attempts. The per-cluster deploy scripts (provider-up.sh /
// consumer-up.sh / central-up.sh / mock-up.sh) are largely idempotent — Liqo
// install checks-then-skips, every kubectl step is `apply` — so re-running one
// after a transient failure is safe. This absorbs the class of intermittent
// network errors (DNS lookup timeouts, an internal etcd port going briefly
// unreachable, IPv6 routes that don't exist on the host) observed once many
// Kind clusters are running concurrently, without forcing a full teardown and
// rebuild of the whole topology over a blip that usually clears in seconds.
func retryDeploy(ctx context.Context, label string, fn func() error) error {
	var lastErr error
	for attempt := 1; attempt <= deployMaxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt < deployMaxAttempts {
			log.Printf("[deploy] %s failed (attempt %d/%d): %v — retrying in %s", label, attempt, deployMaxAttempts, lastErr, deployRetryDelay)
			select {
			case <-time.After(deployRetryDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("%s: giving up after %d attempts: %w", label, deployMaxAttempts, lastErr)
}

func allClusterIDs(opts DeployOpts) []string {
	ids := make([]string, 0, opts.NumConsumers+opts.NumProviders)
	for i := 1; i <= opts.NumConsumers; i++ {
		ids = append(ids, fmt.Sprintf("consumer-%d", i))
	}
	for i := 1; i <= opts.NumProviders; i++ {
		ids = append(ids, fmt.Sprintf("provider-%d", i))
	}
	return ids
}

func deployCentral(ctx context.Context, standaloneDir string, spec ClusterSpec, centralIP string, opts DeployOpts) error {
	log.Printf("[deploy] deploying broker on %s", spec.Name)
	scriptPath := filepath.Join(standaloneDir, "central-up.sh")
	args := []string{
		scriptPath,
		"--public-endpoint", centralIP,
		"--kubeconfig", spec.Kubeconfig,
		"--ca-dir", opts.CADir,
		"--registry", opts.ImgPrefix,
		"--tag", opts.ImgTag,
	}
	return runBashScript(ctx, args...)
}

func deployMocks(ctx context.Context, standaloneDir string, spec ClusterSpec, opts DeployOpts) error {
	log.Printf("[deploy] deploying mock-eco + mock-geo on %s", spec.Name)
	scriptPath := filepath.Join(standaloneDir, "mock-up.sh")
	args := []string{
		scriptPath,
		"--kubeconfig", spec.Kubeconfig,
		"--registry", opts.ImgPrefix,
		"--tag", opts.ImgTag,
	}
	return runBashScript(ctx, args...)
}

func mintJoinBundle(ctx context.Context, standaloneDir, caDir, clusterID, bundleDir string) error {
	log.Printf("[deploy] minting join bundle for %s", clusterID)
	scriptPath := filepath.Join(standaloneDir, "central-up.sh")
	outPath := filepath.Join(bundleDir, clusterID+"-bundle.tgz")
	args := []string{
		scriptPath,
		"join",
		"--cluster-id", clusterID,
		"--ca-dir", caDir,
		"--out", outPath,
	}
	return runBashScript(ctx, args...)
}

func deployConsumer(ctx context.Context, standaloneDir string, spec ClusterSpec, clusterID, bundlePath, consumerIP, mockGeoURL string, opts DeployOpts) error {
	log.Printf("[deploy] deploying consumer %s on %s", clusterID, spec.Name)
	scriptPath := filepath.Join(standaloneDir, "consumer-up.sh")
	args := []string{
		scriptPath,
		"--bundle", bundlePath,
		"--cluster-id", clusterID,
		"--kubeconfig", spec.Kubeconfig,
		"--registry", opts.ImgPrefix,
		"--tag", opts.ImgTag,
		"--public-endpoint", consumerIP,
		"--mock-geo-url", mockGeoURL,
		"--liqo-provider", opts.LiqoProvider,
		"--skip-liqo-dashboard",
		"--skip-cluster-autoscaler",
	}
	if opts.LiqoProvider != liqoProviderKind {
		args = append(args, "--pod-cidr", spec.PodCIDR, "--service-cidr", spec.SvcCIDR)
	}
	return runBashScript(ctx, args...)
}

func deployProvider(ctx context.Context, standaloneDir string, spec ClusterSpec, clusterID, bundlePath, mockEcoURL, mockGeoURL string, opts DeployOpts) error {
	log.Printf("[deploy] deploying provider %s on %s", clusterID, spec.Name)
	scriptPath := filepath.Join(standaloneDir, "provider-up.sh")
	args := []string{
		scriptPath,
		"--bundle", bundlePath,
		"--cluster-id", clusterID,
		"--kubeconfig", spec.Kubeconfig,
		"--registry", opts.ImgPrefix,
		"--tag", opts.ImgTag,
		"--mock-eco-url", mockEcoURL,
		"--mock-geo-url", mockGeoURL,
		"--liqo-provider", opts.LiqoProvider,
	}
	if opts.EcoCacheTTL > 0 {
		args = append(args, "--eco-cache-ttl", opts.EcoCacheTTL.String())
	}
	if opts.LiqoProvider != liqoProviderKind {
		args = append(args, "--pod-cidr", spec.PodCIDR, "--service-cidr", spec.SvcCIDR)
	}
	return runBashScript(ctx, args...)
}

// runBashScript runs one of the deploy/standalone scripts, whose first
// argument is the script path, with its output going to this process's own
// stdout/stderr so a failing deploy is readable in the harness log.
func runBashScript(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "bash", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run bash %s: %w", strings.Join(args[:1], " "), err)
	}
	return nil
}

// WaitForReadiness polls the federation topology until all components are ready.
func WaitForReadiness(ctx context.Context, brokerURL, serverName string, consoleURLs []string, expectedProviders []string, identities map[string]Identity, caFile string, timeout time.Duration) (*ExperimentClients, error) {
	log.Println("[readiness] waiting for all components to come online...")
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("readiness timeout (%s): last error: %v", timeout, lastErr)
		}

		clients, err := tryBuildClients(ctx, brokerURL, serverName, consoleURLs, expectedProviders, identities, caFile)
		if err == nil {
			log.Println("[readiness] all components online")
			return clients, nil
		}
		lastErr = err
		log.Printf("[readiness] not ready yet: %v", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func tryBuildClients(ctx context.Context, brokerURL, serverName string, consoleURLs []string, expectedProviders []string, identities map[string]Identity, caFile string) (*ExperimentClients, error) {
	// Use consumer-1 as the primary identity for shared operations.
	primaryID := identities["consumer-1"]

	primaryBroker, err := NewBrokerClientFromIdentity(brokerURL, serverName, primaryID, caFile)
	if err != nil {
		return nil, fmt.Errorf("build primary broker client: %w", err)
	}

	if err := primaryBroker.CheckReachable(ctx); err != nil {
		return nil, fmt.Errorf("broker not reachable: %w", err)
	}

	ngResp, err := primaryBroker.GetNodeGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("get nodegroups: %w", err)
	}

	advertisingIDs := UniqueProviderIDs(ngResp.NodeGroups)
	advertisingSet := make(map[string]bool, len(advertisingIDs))
	for _, pid := range advertisingIDs {
		advertisingSet[pid] = true
	}
	for _, prov := range expectedProviders {
		if !advertisingSet[prov] {
			return nil, fmt.Errorf("provider %q not yet advertising (have: %v)", prov, advertisingIDs)
		}
	}

	// Build per-consumer broker clients.
	brokers := make(map[string]*BrokerClient, len(identities))
	for cid, id := range identities {
		bc, err := NewBrokerClientFromIdentity(brokerURL, serverName, id, caFile)
		if err != nil {
			return nil, fmt.Errorf("build broker client for %s: %w", cid, err)
		}
		if err := bc.CheckReachable(ctx); err != nil {
			return nil, fmt.Errorf("broker not reachable for %s: %w", cid, err)
		}
		brokers[cid] = bc
		log.Printf("[readiness] broker client for %s (identity=%s): OK", cid, id.ClusterID)
	}

	certFP, _ := primaryID.CertFingerprint()
	consoles := make(map[string]*ConsoleClient, len(consoleURLs))
	for i, url := range consoleURLs {
		cid := fmt.Sprintf("consumer-%d", i+1)
		cc := NewConsoleClient(url)
		if _, err := cc.getState(ctx); err != nil {
			return nil, fmt.Errorf("consumer %s console not ready at %s: %w", cid, url, err)
		}
		consoles[cid] = cc
	}

	return &ExperimentClients{
		Identity:   primaryID,
		CertFP:     certFP,
		Broker:     primaryBroker,
		Brokers:    brokers,
		Consoles:   consoles,
		InitialNGs: ngResp,
	}, nil
}

// deployControllableMockEco builds the controllable mock-eco image, loads it
// into the central Kind cluster, and patches the mock-eco Deployment to use it.
func deployControllableMockEco(ctx context.Context, repoRoot string, centralSpec ClusterSpec) error {
	imgName := "federation-autoscaler/mock-eco-test:latest"

	log.Println("[deploy] building controllable mock-eco image")
	cmd := exec.CommandContext(ctx, "docker", "build",
		"-t", imgName,
		"-f", filepath.Join(repoRoot, "federation-tests", "mock-eco-test", "Dockerfile"),
		repoRoot,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build mock-eco-test image: %w", err)
	}

	log.Println("[deploy] loading controllable mock-eco into central cluster")
	if err := KindLoadImages(ctx, centralSpec.Name, []string{imgName}); err != nil {
		return fmt.Errorf("load mock-eco-test image: %w", err)
	}

	log.Println("[deploy] patching mock-eco Deployment to use controllable version")
	patchJSON := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"mock-eco","image":"%s","args":["--port=8081"]}]}}}}`, imgName)
	cmd = exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", centralSpec.Kubeconfig,
		"-n", "federation-autoscaler-system",
		"patch", "deploy/mock-eco",
		"--type=strategic",
		"-p", patchJSON,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("patch mock-eco deployment: %w", err)
	}

	log.Println("[deploy] waiting for mock-eco rollout")
	cmd = exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", centralSpec.Kubeconfig,
		"-n", "federation-autoscaler-system",
		"rollout", "status", "deploy/mock-eco",
		"--timeout=120s",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// regionLocation maps region codes to the coordinates mock-geo would return.
// Used by registerGeoOverrides to tell the controllable mock-geo what location
// to return for each cluster's Docker bridge IP.
type regionLoc struct {
	City string
	Lat  float64
	Lon  float64
}

// RegionLocation returns the city and coordinates mock-geo reports for a region
// code, and whether the code is known at all. A config naming an unknown region
// fails deep inside deployment (registerGeoOverrides); checking it up front
// fails in seconds.
func RegionLocation(code string) (city string, lat, lon float64, ok bool) {
	loc, ok := regionLocation[code]
	return loc.City, loc.Lat, loc.Lon, ok
}

var regionLocation = map[string]regionLoc{
	"QC":  {"Montreal", 45.6085, -73.5493},
	"CA":  {"San Jose", 37.3382, -121.8863},
	"NSW": {"Sydney", -33.8688, 151.2093},
	"IDF": {"Paris", 48.8566, 2.3522},
	"ENG": {"London", 51.5074, -0.1278},
	"13":  {"Tokyo", 35.6895, 139.6917},
	"HE":  {"Frankfurt", 50.1109, 8.6821},
	"SP":  {"Sao Paulo", -23.5505, -46.6333},
	"MH":  {"Mumbai", 19.0760, 72.8777},
	"AB":  {"Stockholm", 59.3293, 18.0686},
	"LOM": {"Milan", 45.4642, 9.1900},
	"SG":  {"Singapore", 1.3521, 103.8198},
	"VA":  {"Ashburn", 38.7223, -77.0193},
	"TX":  {"Dallas", 32.7767, -96.7970},
	"OR":  {"Portland", 45.5051, -122.6750},
	"IE":  {"Dublin", 53.3498, -6.2603},
	"NL":  {"Amsterdam", 52.3676, 4.9041},
	"KR":  {"Seoul", 37.5665, 126.9780},
	"ZA":  {"Johannesburg", -26.2041, 28.0473},
	"AE":  {"Dubai", 25.2048, 55.2708},
	"IL":  {"Chicago", 41.8781, -87.6298},
	"OH":  {"Columbus", 39.9612, -82.9988},
	"GA":  {"Atlanta", 33.7490, -84.3880},
	"BC":  {"Vancouver", 49.2827, -123.1207},
	"BY":  {"Munich", 48.1351, 11.5820},
	"RM":  {"Rome", 41.9028, 12.4964},
	"CT":  {"Barcelona", 41.3874, 2.1686},
	"AN":  {"Ankara", 39.9334, 32.8597},
	"VIC": {"Melbourne", -37.8136, 144.9631},
	"NCL": {"Auckland", -36.8485, 174.7633},
	"HK":  {"Hong Kong", 22.3193, 114.1694},
	"TW":  {"Taipei", 25.0330, 121.5654},
	"PH":  {"Manila", 14.5995, 120.9842},
	"CL":  {"Santiago", -33.4489, -70.6693},
	"CO":  {"Bogota", 4.7110, -74.0721},
	"MX":  {"Mexico City", 19.4326, -99.1332},
	"FI":  {"Helsinki", 60.1699, 24.9384},
	"NO":  {"Oslo", 59.9139, 10.7522},
	"DK":  {"Copenhagen", 55.6761, 12.5683},
	"PL":  {"Warsaw", 52.2297, 21.0122},
	"CZ":  {"Prague", 50.0755, 14.4378},
	"AT":  {"Vienna", 48.2082, 16.3738},
	"CH":  {"Zurich", 47.3769, 8.5417},
	"BE":  {"Brussels", 50.8503, 4.3517},
	"PT":  {"Lisbon", 38.7223, -9.1393},
	"GR":  {"Athens", 37.9838, 23.7275},
	"RO":  {"Bucharest", 44.4268, 26.1025},
	"IL2": {"Tel Aviv", 32.0853, 34.7818},
	"EG":  {"Cairo", 30.0444, 31.2357},
	"NG":  {"Lagos", 6.5244, 3.3792},
}

// registerGeoOverrides calls the controllable mock-geo admin API to map each
// cluster's real Docker bridge IP to its configured region. This lets agents
// keep their real (routable) IPs for probe endpoints while mock-geo returns
// the correct region for geo discovery.
func registerGeoOverrides(ctx context.Context, mockGeoURL string, opts DeployOpts) error {
	if len(opts.ProviderRegions) == 0 {
		return nil
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}

	// Register each provider's node IPs. Kind clusters with a worker node have
	// two Docker containers (control-plane + worker) with different IPs. The
	// agent can be scheduled on either, so register both.
	for i := 0; i < opts.NumProviders; i++ {
		region := ""
		if i < len(opts.ProviderRegions) {
			region = opts.ProviderRegions[i]
		}
		if region == "" {
			continue
		}
		loc, ok := regionLocation[region]
		if !ok {
			return fmt.Errorf("provider-%d: unknown region %q", i+1, region)
		}

		spec := opts.Specs[1+opts.NumConsumers+i]
		for _, suffix := range nodeContainerSuffixes(spec) {
			containerName := spec.Name + suffix
			ip, err := ContainerIP(ctx, containerName)
			if err != nil {
				log.Printf("[deploy] mock-geo: skip %s (no IP): %v", containerName, err)
				continue
			}
			if err := postGeoOverride(ctx, httpClient, mockGeoURL, ip, region, loc); err != nil {
				return fmt.Errorf("register geo for %s (%s): %w", containerName, ip, err)
			}
			log.Printf("[deploy] mock-geo: %s (%s) → %s (%s)", ip, containerName, region, loc.City)
		}
	}

	// Register each consumer's node IPs (needed for latency distance calculation).
	for i := 0; i < opts.NumConsumers; i++ {
		spec := opts.Specs[1+i]
		region := opts.ProviderRegions[0]
		loc := regionLocation[region]
		for _, suffix := range nodeContainerSuffixes(spec) {
			containerName := spec.Name + suffix
			ip, err := ContainerIP(ctx, containerName)
			if err != nil {
				log.Printf("[deploy] mock-geo: skip %s (no IP): %v", containerName, err)
				continue
			}
			if err := postGeoOverride(ctx, httpClient, mockGeoURL, ip, region, loc); err != nil {
				return fmt.Errorf("register geo for %s (%s): %w", containerName, ip, err)
			}
			log.Printf("[deploy] mock-geo: %s (%s, consumer-%d) → %s (%s)", ip, containerName, i+1, region, loc.City)
		}
	}

	return nil
}

// nodeContainerSuffixes returns the Docker container name suffixes for a Kind
// cluster. A cluster with a worker has two containers; without, just one.
func nodeContainerSuffixes(spec ClusterSpec) []string {
	if spec.HasWorker {
		return []string{"-control-plane", "-worker"}
	}
	return []string{"-control-plane"}
}

// postGeoOverride POSTs one IP→region override to the controllable mock-geo.
// Retries on connection errors for a short window: kubectl rollout status
// reports the mock-geo Deployment Available as soon as the pod is Ready, but
// the Service's NodePort routing (kube-proxy iptables) can lag a moment
// behind that under load — observed as "connect: connection refused" on the
// very first call right after rollout, especially with many Kind clusters
// starting up concurrently.
func postGeoOverride(ctx context.Context, client *http.Client, mockGeoURL, ip, region string, loc regionLoc) error {
	body := fmt.Sprintf(`{"ip":"%s","region":"%s","city":"%s","lat":%f,"lon":%f}`,
		ip, region, loc.City, loc.Lat, loc.Lon)

	const maxAttempts = 15
	const retryDelay = 2 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			mockGeoURL+"/admin/geo", strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < maxAttempts {
				log.Printf("[deploy] mock-geo: %s not yet reachable (attempt %d/%d): %v", mockGeoURL, attempt, maxAttempts, err)
				if sleepErr := sleepOrCtx(ctx, retryDelay); sleepErr != nil {
					return sleepErr
				}
				continue
			}
			return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("admin/geo returned %d", resp.StatusCode)
		}
		return nil
	}
	return lastErr
}

// sleepOrCtx sleeps for d, or returns ctx.Err() early if ctx is cancelled first.
func sleepOrCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// deployControllableMockGeo builds the controllable mock-geo image, loads it
// into the central Kind cluster, and patches the mock-geo Deployment to use it.
func deployControllableMockGeo(ctx context.Context, repoRoot string, centralSpec ClusterSpec) error {
	imgName := "federation-autoscaler/mock-geo-test:latest"

	log.Println("[deploy] building controllable mock-geo image")
	cmd := exec.CommandContext(ctx, "docker", "build",
		"-t", imgName,
		"-f", filepath.Join(repoRoot, "federation-tests", "mock-geo-test", "Dockerfile"),
		repoRoot,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build mock-geo-test image: %w", err)
	}

	log.Println("[deploy] loading controllable mock-geo into central cluster")
	if err := KindLoadImages(ctx, centralSpec.Name, []string{imgName}); err != nil {
		return fmt.Errorf("load mock-geo-test image: %w", err)
	}

	log.Println("[deploy] patching mock-geo Deployment to use controllable version")
	patchJSON := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"mock-geo","image":"%s","args":["--port=8080"]}]}}}}`, imgName)
	cmd = exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", centralSpec.Kubeconfig,
		"-n", "federation-autoscaler-system",
		"patch", "deploy/mock-geo",
		"--type=strategic",
		"-p", patchJSON,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("patch mock-geo deployment: %w", err)
	}

	log.Println("[deploy] waiting for mock-geo rollout")
	cmd = exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", centralSpec.Kubeconfig,
		"-n", "federation-autoscaler-system",
		"rollout", "status", "deploy/mock-geo",
		"--timeout=120s",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SetConsumerOllama points a consumer agent at an Ollama server for the
// ConsumerChoice policy: it writes ollamaUrl / ollamaModel / ollamaTimeout into
// agent-config (the same keys consumer-up.sh --ollama-url sets), restarts the
// agent so its flags pick them up, and waits for the rollout. url must be
// reachable from inside the consumer cluster.
func SetConsumerOllama(ctx context.Context, kubeconfig, url, model string, timeout time.Duration) error {
	patch, err := json.Marshal(map[string]map[string]string{"data": {
		"ollamaUrl": url, "ollamaModel": model, "ollamaTimeout": timeout.String(),
	}})
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		{"patch", "configmap", "agent-config", "--type", "merge", "-p", string(patch)},
		{"rollout", "restart", "deploy/agent"},
		{"rollout", "status", "deploy/agent", "--timeout=300s"},
	} {
		full := append([]string{"--kubeconfig", kubeconfig, "-n", "federation-autoscaler-system"}, args...)
		cmd := exec.CommandContext(ctx, "kubectl", full...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("kubectl %s: %w", strings.Join(args[:2], " "), err)
		}
	}
	return nil
}

// SetProviderPrices writes a provider's per-resource unit prices (per unit per
// hour, e.g. {"cpu": "0.03", "memory": "0.004"}) into the agent-prices
// ConfigMap the agent reads via --price-file. The Broker only derives a
// per-chunk Cost when every resource of the chunk is priced, so pass at least
// cpu and memory. Like SetProviderCapacity it takes effect on the agent's next
// file refresh and advertisement, not immediately.
func SetProviderPrices(ctx context.Context, kubeconfig string, prices map[string]string) error {
	names := make([]string, 0, len(prices))
	for name := range prices {
		names = append(names, name)
	}
	sort.Strings(names) // stable file content across runs
	var sb strings.Builder
	for _, name := range names {
		fmt.Fprintf(&sb, "%s: %q\n", name, prices[name])
	}
	patch := fmt.Sprintf(`{"data":{"prices.yaml":%q}}`, sb.String())
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"-n", "federation-autoscaler-system",
		"patch", "configmap", "agent-prices",
		"--type", "merge",
		"-p", patch,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SetProviderCapacity patches a provider's agent-capacity ConfigMap to a
// fixed cpu/memory cap (see config/agent/provider/capacity-configmap.yaml),
// which the broker's chunk sizer floor-divides into standard 2-CPU/4-GiB
// chunks. Without this, a provider's advertised capacity defaults to its
// Kind node's real allocatable — on a shared host that's the host's own
// (large, unthrottled) CPU/RAM, so every provider ends up with far more
// chunks than a handful of test consumers could ever exhaust, and the
// cheapest/greenest provider never actually runs out of room for everyone
// to land on it. Capping it small enough to be exhausted lets Eco/Latency's
// second- and third-best choices actually get exercised.
func SetProviderCapacity(ctx context.Context, kubeconfig string, cpu, memory string) error {
	capacityYAML := fmt.Sprintf("cpu: %q\nmemory: %q\n", cpu, memory)
	patch := fmt.Sprintf(`{"data":{"capacity.yaml":%q}}`, capacityYAML)
	cmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig,
		"-n", "federation-autoscaler-system",
		"patch", "configmap", "agent-capacity",
		"--type", "merge",
		"-p", patch,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
