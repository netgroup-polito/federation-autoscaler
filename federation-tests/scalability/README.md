# Broker Scalability Test Harness

Automated scalability experiment for the federation-autoscaler **Broker** API.
Generates synthetic Consumer and Provider HTTP traffic without deploying real
agents, clusters, controllers, Liqo tunnels, or Cluster Autoscaler instances.

## Running it

Set how many agents to simulate in
[`federation-tests/configs/scalability.yaml`](../configs/scalability.yaml) — the only two
settings that change between runs:

```yaml
consumers: 5
providers: 5
```

Then, from the repository root (on Linux, for the Broker's CPU/RAM samples):

```bash
bash federation-tests/scalability/run-scalability-test.sh
```

The script does everything: builds the Broker and the harness, generates the
certificates, creates a Kind cluster for the Broker's CRDs, starts the Broker,
runs the test, cleans up the test CRs and deletes the cluster. Options:

| Option | Effect |
|---|---|
| `--config FILE` | Use another YAML file instead of `federation-tests/configs/scalability.yaml` |
| `--keep-cluster` | Keep the Kind cluster afterwards, so the next run skips creating it |

The file accepts `consumers` and `providers` only (whole numbers, at least one
above 0); any other key is an error. Everything else is fixed in the script, so
every run measures the same thing and only the load changes:

| Setting | Value |
|---|---|
| Measurement duration | 5 min, after the warm-up |
| Provider advertisement / instruction poll | every 30 s / 5 s |
| Consumer heartbeat / evaluation / instruction poll | every 15 s / 5 s / 5 s |
| Per-attempt request timeout | 10 s, with the agent client's 3 retries (as in production) |
| Synthetic provider size | 16 CPU, 32 GiB |
| Broker CPU/RAM sampling | every 5 s from `/proc` (Linux only; skipped elsewhere) |
| Broker API port | 9444 |

> **On the instruction-poll interval.** The harness polls every 5 s, which is
> what the agents defaulted to when the published numbers were measured. The
> agent default is now 1 s (`--poll-interval`, `FA_POLL_INTERVAL`), so a
> deployment left at the default sends five times as many instruction polls as
> this measures. To measure that instead, pass
> `--instruction-poll-interval 1s` to the harness binary (see
> [running the harness by hand](#advanced-running-the-harness-by-hand)).

Results go to `results/scalability/<UTC timestamp>/`, together with a copy of
the YAML file used (`config.yaml`). In `summary.md`, check that:

- the error rate is 0 and the Cancelled column is 0;
- the Broker resource section has samples, with CPU in cores;
- each operation's attempts are about agents × 300 s / interval — e.g. about
  60 evaluations per consumer. Far fewer means the harness fell behind.

To see how the Broker behaves as the load grows, run it once per scale,
changing the two numbers each time (see [Scale Configurations](#scale-configurations)).

## Scope

### What it tests

| Metric | Description |
|---|---|
| Evaluation latency | `GET /api/v1/nodegroups` round-trip time |
| Advertisement throughput | `POST /api/v1/advertisements` processing rate |
| Heartbeat throughput | `POST /api/v1/heartbeat` processing rate |
| Instruction poll rate | `GET /api/v1/instructions` processing rate |
| Broker CPU/RAM | Resource usage during the test |

### What it does NOT test

Real Consumer/Provider Agent instances, Kubernetes clusters, controllers,
Cluster Autoscaler, Liqo peering, virtual nodes, tunnels, certificate exchange,
reservations, or resource allocation.

### Kubernetes persistence note

The Broker **is** a controller-runtime process. Provider advertisements
create/update `ClusterAdvertisement` CRs in etcd via the Broker's existing
Kubernetes-backed persistence path. The test does not deploy additional
clusters, agents, or controllers, but it **does** exercise the Broker's real
CRD write path. See [Cleanup](#cleanup) for how to remove test-created CRs.

## Traffic Pattern (matches real system)

| Agent type | Endpoint | Method | Interval | Purpose |
|---|---|---|---|---|
| **Provider** | `/api/v1/advertisements` | POST | 30 s | Liveness + resource data (doubles as heartbeat) |
| **Provider** | `/api/v1/instructions` | GET | 5 s | Instruction poll |
| **Consumer** | `/api/v1/heartbeat` | POST | 15 s | Liveness + policy + location |
| **Consumer** | `/api/v1/nodegroups` | GET | configurable | Evaluation (the metric under test) |
| **Consumer** | `/api/v1/instructions` | GET | 5 s | Instruction poll |

The cadences above are the harness's own; the agent's current default
instruction poll is 1 s (see the note in [Running it](#running-it)).

> **Note:** `POST /api/v1/heartbeat` is Consumer-only. Providers do NOT call
> this endpoint — their 30 s advertisement POST serves as their liveness signal.
> `POST /api/v1/reservations` is explicitly excluded.

**Which placement policy is measured.** Every consumer registers with the
`Random` placement policy, so `GET /api/v1/nodegroups` is measured on that one
policy. Most of what that request costs is common to every policy — listing
the advertisements, computing each provider's per-chunk cost and carbon
intensity, checking the consumer's in-flight reservations — and all of it is
measured. What differs is the final step, and here it is Random's (one random
pick): Price and Eco sort the providers instead, Latency also computes a
distance to every provider, and ConsumerChoice skips the step entirely.

## Prerequisites

With `run-scalability-test.sh`, on the machine that runs the test:

- `go` (the version in `go.mod`; the script refuses anything older than 1.22),
  `docker` (running), `kind`, `kubectl`, `openssl` and `curl` on `PATH`; the script
  checks them first and stops if one is missing;
- Linux, for the Broker's CPU/RAM samples (elsewhere the run goes on without them).

The script starts the Broker itself. Only when running the harness by hand (below) do
you need a **running Broker** reachable over HTTPS with mTLS.

The machine the published results come from, and the commands to describe yours, are in
section 15 of the [test guide](../guide.md#15-environment-the-tests-were-run-on).

## Authentication: mTLS certificates

The Broker enforces `body.clusterId == TLS cert CN` on every request via its
`ClusterIDMiddleware`. Each logical agent **must** present a unique client
certificate whose Common Name (CN) matches the `clusterId` in the request body.

### Generating test certificates

The included `generate-test-certs.sh` script creates a disposable test-only CA
and per-agent client certificates:

```bash
cd federation-tests/scalability
./generate-test-certs.sh \
  --consumers 50 \
  --providers 100 \
  --out-dir certs/run-001

# Creates:
#   certs/run-001/ca.crt, ca.key          — test CA (NOT the production CA)
#   certs/run-001/server.crt, server.key  — server cert (for local test broker)
#   certs/run-001/scaltest-provider-001.{crt,key}  — per-provider certs
#   certs/run-001/scaltest-consumer-001.{crt,key}  — per-consumer certs
```

For a **production Broker**, use `deploy/standalone/central-up.sh join` to mint
per-agent bundles from the real federation CA instead.

### TLS operating modes

| Mode | Flags | When to use |
|---|---|---|
| **Per-agent certs** | `--certs-dir <dir>` | Points to `generate-test-certs.sh` output. Each agent uses its own cert. CA is read from `<dir>/ca.crt`. |
| **Single shared cert** | `--tls-cert ... --tls-key ... --tls-ca ...` | Only valid with ≤1 provider + ≤1 consumer (CN must match body.clusterId). |
| **ServerName override** | above + `--broker-server-name <host>` | When dialing via `kubectl port-forward` to localhost but the server cert's SAN only covers in-cluster DNS names. |

> **Plain HTTP will NOT work** with the production Broker — its API listener is
> HTTPS-only with `RequireAndVerifyClientCert`. The Broker's TLS configuration
> has no insecure-skip-verify option; every connection must present a valid
> client certificate and verify the server certificate against the CA bundle.

## Advanced: running the harness by hand

[Running it](#running-it) is the normal way. The steps below drive the harness
binary directly, against a Broker you started yourself, with every flag
available (see [CLI Flags](#cli-flags)).

### Build

```bash
cd federation-tests/scalability
go build -o bin/broker-scalability-test .
```

### Generate test certs + smoke test

```bash
# Generate certs for 1 consumer + 1 provider
./generate-test-certs.sh --consumers 1 --providers 1 --out-dir certs/smoke

# Run 60-second smoke test against a locally port-forwarded Broker
# (--broker-server-name overrides TLS verification when the server cert
#  doesn't cover "localhost")
./bin/broker-scalability-test \
  --consumers 1 --providers 1 --duration 60s \
  --certs-dir certs/smoke \
  --broker-url https://localhost:9443 \
  --broker-server-name broker.federation-autoscaler-system.svc \
  --output-dir results/smoke-001
```

### Full 10-minute experiment (50 consumers, 100 providers)

```bash
./generate-test-certs.sh --consumers 50 --providers 100 --out-dir certs/full

./bin/broker-scalability-test \
  --consumers 50 --providers 100 --duration 10m \
  --certs-dir certs/full \
  --consumer-eval-interval 5s \
  --advertisement-interval 30s \
  --heartbeat-interval 15s \
  --instruction-poll-interval 5s \
  --broker-url https://broker.example.com:9443 \
  --output-dir results/full-150-10m
```

## Test Lifecycle

1. Validate parameters and output directory
2. Check Broker reachability (`GET /healthz`)
3. Save full configuration to `configuration.json`
4. Start Broker CPU/RAM monitoring
5. **Warm-up phase:** Start all provider advertisement loops. Wait until every
   provider has successfully advertised at least once (HTTP 200). This ensures
   `GET /api/v1/nodegroups` returns a non-empty provider list.
6. Start all consumer heartbeat loops. Wait until every consumer has heartbeated
   once.
7. **Measurement phase:** Start consumer evaluation (`GET /api/v1/nodegroups`)
   traffic.
8. Run for the configured duration.
9. Stop in two steps: no new request starts once the duration is over, but a
   request already in flight completes normally (up to `--request-timeout`);
   only then are the generators and the monitor cancelled.
10. Save raw metrics and generate summary.

### Agents are not in lockstep

Every loop of every agent (advertisement, heartbeat, instruction poll,
evaluation) starts at its own random point of its first interval, then keeps
its interval. Started together without this, all agents would fire in the
same instant and the Broker would see bursts of N simultaneous requests
followed by silence — something independently started real agents never do.
The offsets derive from `--seed`, so the same seed reproduces the same
schedule. Evaluations are spread over the first interval after the
measurement starts.

### What counts as an error

A request's outcome is `success`, `failure` (an HTTP error or a connection
problem), `timeout` (no answer within `--request-timeout`) or `cancelled`.
`cancelled` means the harness itself stopped the request (Ctrl+C): it is kept
in the CSVs but is neither an attempt nor an error in the summary, because
the Broker never got the chance to answer it. The normal end of a run cancels
nothing (step 9).

**Retries are on, as in production.** The harness sends its requests through
the real agent client (`internal/agent/client`), which retries a transient
failure (a connection error, a timeout, an HTTP 5xx — not a 429 or any other
4xx) of an idempotent call up to 3 times with exponential backoff — every call
this harness makes is idempotent. A `timeout` outcome therefore means every
attempt timed out. The client treats any `--client-max-retries` of 0 or less
as that default, so a single attempt is not possible without changing the
production client. Two consequences for reading the results:

- a request that fails and then succeeds on a retry is **one success**: the
  transient failure does not appear in the error rate. The error rate counts
  requests that failed even after every retry — what a real agent would see;
- the latency of such a request **includes** the failed attempts and the
  backoff between them, so a Broker that errors transiently shows up as
  higher latency percentiles rather than as errors.

## Warm-up Phase

Provider advertisements must be registered before consumer evaluations begin.
Otherwise `GET /api/v1/nodegroups` returns an empty list and the benchmark
does not reflect normal behavior.

The warm-up blocks until every provider has received at least one HTTP 200
from `POST /api/v1/advertisements`. A configurable `--warmup-timeout`
(default: 60 s) caps how long the warm-up waits.

## Scale Configurations

A curve of the Broker's behaviour against load is one run per point, each
with the two numbers in `federation-tests/configs/scalability.yaml` set as below and the
same fixed 5-minute measurement. `--keep-cluster` saves recreating the Kind
cluster between points; delete it at the end with
`kind delete cluster --name scaltest`.

| Scale | `consumers` | `providers` | Agents | Evaluations per run (≈) |
|---|---|---|---|---|
| Small | 5 | 5 | 10 | 300 |
| Medium | 10 | 10 | 20 | 600 |
| Large | 25 | 25 | 50 | 1 500 |
| XL | 50 | 50 | 100 | 3 000 |
| XXL | 100 | 100 | 200 | 6 000 |

Evaluations per run = consumers × 300 s / 5 s. Each run restarts the Broker
and removes the previous run's test CRs, so the points do not affect one
another.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--consumers` | 1 | Number of logical consumers |
| `--providers` | 1 | Number of logical providers |
| `--duration` | 60s | Total test duration (measurement phase) |
| `--broker-url` | — (required) | Broker REST API base URL (https only), e.g. `https://localhost:9443` |
| `--broker-server-name` | — | TLS ServerName override (for port-forward / SAN mismatch) |
| `--certs-dir` | — | Directory with per-agent certs from generate-test-certs.sh |
| `--tls-cert` | — | Single shared client cert (alternative to --certs-dir; ≤1 provider + ≤1 consumer) |
| `--tls-key` | — | Single shared client key |
| `--tls-ca` | — | CA certificate for server verification (required with --tls-cert) |
| `--output-dir` | `results/<timestamp>` | Output directory |
| `--consumer-eval-interval` | 5s | Interval between consumer evaluations |
| `--advertisement-interval` | 30s | Provider advertisement interval |
| `--heartbeat-interval` | 15s | Consumer heartbeat interval |
| `--instruction-poll-interval` | 5s | Instruction poll interval |
| `--monitor-interval` | 5s | Broker resource monitoring interval |
| `--instruction-poll` | true | Exercise `GET /api/v1/instructions` from every agent |
| `--monitor-mode` | none | `none`, `k8s`, `docker`, `process` |
| `--broker-container` | — | Docker container name (docker mode) |
| `--broker-pod` | — | Exact pod name, overrides --broker-pod-label (k8s mode) |
| `--broker-pod-label` | `app.kubernetes.io/component=broker` | Label selector for `kubectl top pod` (k8s mode) |
| `--broker-namespace` | `federation-autoscaler-system` | K8s namespace (k8s mode + cleanup) |
| `--broker-pid` | 0 | PID of a local `broker` process (process mode; `run-scalability-test.sh` passes it) |
| `--kubeconfig` | — | Kubeconfig path for kubectl (k8s mode + cleanup) |
| `--request-timeout` | 10s | Per-HTTP-attempt timeout |
| `--client-max-retries` | 0 | Additional retry attempts on transient failures. 0 = the agent client's default, 3 retries, as in production (see "What counts as an error") |
| `--warmup-timeout` | 60s | Max time to wait for warm-up |
| `--provider-cpu` | `16` | Synthetic CPU quantity each provider advertises |
| `--provider-memory` | `32Gi` | Synthetic memory quantity each provider advertises |
| `--seed` | current time | Deterministic seed for synthetic data generation |

## Output Files

| File | Format | Contents |
|---|---|---|
| `configuration.json` | JSON | Full experiment configuration |
| `raw_evaluations.csv` | CSV | Per-request evaluation (GET /nodegroups) metrics |
| `raw_provider_requests.csv` | CSV | Per-request provider (POST /advertisements + GET /instructions) metrics |
| `raw_consumer_requests.csv` | CSV | Per-request consumer (POST /heartbeat + GET /instructions) metrics |
| `broker_resource_usage.csv` | CSV | Periodic CPU/RAM samples |
| `summary.json` | JSON | Machine-readable summary |
| `summary.md` | Markdown | Human-readable summary |

## Cleanup

All test-created `ClusterAdvertisement` CRs are named after their advertising
cluster ID, which always starts with the prefix `scaltest-` (e.g.
`scaltest-provider-001`). The cleanup subcommand matches CRs by this **name
prefix** — no labels are involved because the Broker's `upsertClusterAdvertisement()`
sets only `.Spec`, never `.ObjectMeta.Labels`.

```bash
# Dry-run: list CRs that would be deleted
./bin/broker-scalability-test cleanup \
  --broker-namespace federation-autoscaler-system

# Actually delete them
./bin/broker-scalability-test cleanup \
  --broker-namespace federation-autoscaler-system --yes

# Or manually:
kubectl get clusteradvertisements.broker.federation-autoscaler.io \
  -n federation-autoscaler-system -o name \
  | grep '/scaltest-' \
  | xargs kubectl delete -n federation-autoscaler-system
```

The cleanup **never** deletes CRs whose name does not start with `scaltest-`.

## Ctrl+C / Signal Handling

Press Ctrl+C during the test. The harness:
1. Stops all load generators gracefully (context cancellation)
2. Stops the resource monitor
3. Saves all data collected up to the interruption point
4. Generates the summary with actual elapsed duration

## Limitations

1. **mTLS required.** The production Broker requires `RequireAndVerifyClientCert`.
   Use `generate-test-certs.sh` for test certs or production bundles for real certs.
2. **CRD writes.** Provider advertisements create real `ClusterAdvertisement` CRs.
   Use `cleanup` after testing or use a disposable cluster.
3. **Instruction poll returns empty.** Without controllers creating instructions,
   `GET /api/v1/instructions` returns `[]`. The endpoint is still exercised to
   measure request processing overhead.
4. **No reservations.** `POST /api/v1/reservations` is not called by design.
5. **Resource monitoring** requires access to the Broker process. Use
   `--monitor-mode none` if unavailable. `--monitor-mode process` reads the
   Broker's `/proc/<pid>/stat` and `/proc/<pid>/status`, so it works on Linux
   only: CPU is the process's CPU time between two samples over the wall time
   between them, in cores (1.0 = one core busy for the whole interval), and
   memory is its resident set size. A wrong PID or a system without `/proc`
   fails before the run starts. (`ps %cpu` is not used: it is the average
   over the whole life of the process, not the load of the moment.)
6. **Run on Linux for sub-millisecond latencies.** On Windows the clock the
   harness times requests with moves in steps of about 0.5 ms, so the fastest
   requests read as 0.0 ms or 0.5 ms.
