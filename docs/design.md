# Dynamic Multi-Provider Cluster Autoscaling for the Computing Continuum

## Architectural Proposal (as-built)

**Version:** 3.2

**Status:** alpha — the design has shipped end-to-end across the broker, agents, gRPC server, and the four-cluster e2e suite (see §11 and the README "Status" table). The numbered sections below each carry an "Implemented in:" footer pointing at the canonical Go packages, kustomize overlays, or YAML samples that realise the spec.

**Based on:** "Dynamic Multi-Provider Cluster Autoscaling For The Computing Continuum" (ACM SAC 2025)

**Integrates:** k8s-resource-brokering, Multi-Cluster-Autoscaler, Kubernetes Cluster Autoscaler, Liqo

---

## Table of Contents

1. [Problem Statement](#1-problem-statement)
2. [Goals and Non-Goals](#2-goals-and-non-goals)
3. [System Overview](#3-system-overview)
4. [Components](#4-components)
5. [Custom Resource Definitions (CRDs)](#5-custom-resource-definitions-crds)
6. [ConfigMap Definition](#6-configmap-definition)
7. [API Contracts](#7-api-contracts)
8. [Execution Flows](#8-execution-flows)
9. [Failure Handling and Reconciliation](#9-failure-handling-and-reconciliation)
10. [Security](#10-security)
11. [Integration Plan with Existing Projects](#11-integration-plan-with-existing-projects)

---

## 1. Problem Statement

A single Kubernetes Cluster Autoscaler (CA) instance supports exactly **one cloud provider** at a time. It cannot natively borrow compute capacity from multiple heterogeneous sources (AWS, Azure, bare-metal, edge clusters) simultaneously.

Organizations operating in the computing continuum — spanning cloud, edge, and on-premise — need a mechanism to:

- Dynamically borrow resources from multiple existing Kubernetes clusters across different providers
- Make intelligent placement decisions based on cost, latency, and resource availability
- Present borrowed remote resources as regular nodes to the consumer cluster's scheduler
- Scale down and release borrowed resources when no longer needed
- **Operate even when consumer and provider clusters are behind NAT, firewalls, or otherwise unreachable from the internet** — only the central Broker requires a publicly reachable endpoint

This proposal defines a complete architecture that achieves this by combining **Kubernetes Cluster Autoscaler** (unmodified, vanilla), a **custom gRPC Cloud Provider**, a **central Resource Broker**, a per-cluster **Consumer Agent** or **Provider Agent** (one role per cluster), and **Liqo** for multi-cluster networking and virtual node creation.

---

## 2. Goals and Non-Goals

### Goals

- **G1:** Enable a single consumer cluster to scale across multiple provider clusters from different infrastructure providers simultaneously
- **G2:** Use the Kubernetes Cluster Autoscaler without any modifications to its source code
- **G3:** Centralize resource decision-making in a broker that considers cost, availability, and consumer-specific priorities
- **G4:** Support both standard (CPU/memory) and GPU workloads
- **G5:** Use Liqo to establish multi-cluster peering and present remote resources as virtual nodes
- **G6:** Support multiple consumers sharing the same provider cluster through resource chunking
- **G7:** Handle failures gracefully with timeouts, heartbeats, and reconciliation loops
- **G8:** Provide a secure communication channel between all components using mTLS
- **G9:** **Agent-initiated communication only**: the Broker never opens a connection toward a consumer or provider cluster. Every byte that reaches an agent is the response body of an HTTP call the agent initiated. Consumers and providers therefore need only outbound egress to the Broker's URL.
- **G10:** The gRPC server never talks to the Broker directly — it delegates every cross-cluster interaction to its co-located Consumer Agent over an in-cluster REST API.

### Non-Goals

- Provisioning new VMs or physical machines (only borrows from existing clusters)
- Modifying the Cluster Autoscaler source code
- Supporting a cluster that is simultaneously a consumer and a provider
- Real-time sub-second autoscaling (target: 15-30 second scale-up latency)
- **Multi-criteria (weighted) placement scoring.** The Broker implements placement strategies a consumer opts into via a `ConsumerPolicy` — **Price** (cheapest), **Eco** (lowest carbon intensity), **Latency** (lowest **measured** RTT among the nearest few) — each ranking on one advertised metric (see §4.1, §5.6). Latency is a two-stage refinement: the Broker shortlists the top-N nearest by Haversine distance, then the Consumer Agent UDP-probes them and grows the lowest-RTT one. What remains out of scope: *combining* metrics into a single weighted multi-criteria score. The selection point (per-consumer node-group masking) is where richer scoring would plug in.
- Broker-initiated (push) delivery of any kind — explicitly out of scope

---

## 3. System Overview

![Architecture Diagram](diagrams/architecture-diagram.png)

> **Source-of-truth diagram:** [`diagrams/architecture.mmd`](diagrams/architecture.mmd). The PNG is a rendering — regenerate via `mmdc` per [`diagrams/README.md`](diagrams/README.md).

The system consists of three deployment domains connected by a strict asymmetric communication model:

| Channel | Initiator | Style | Rationale |
| --- | --- | --- | --- |
| CA ↔ gRPC Server | CA | gRPC, in-cluster | upstream `externalgrpc.proto` contract |
| gRPC Server ↔ Consumer Agent | gRPC Server | REST, in-cluster, **push-synchronous** | fast, low-latency local loop |
| Consumer Agent ↔ Broker | **Consumer Agent only** | REST over mTLS, **5 s polling + sync POSTs** (heartbeat, reservations) | works from any NATed consumer cluster with egress to the Broker |
| Provider Agent ↔ Broker | **Provider Agent only** | REST over mTLS, **5 s polling + sync POSTs** (30 s advertisements) | works from any NATed provider cluster with egress to the Broker |
| Agent → Mock services | **Agent only** | plain HTTP, **agent-initiated** GETs (mock-geo IP-keyed, mock-eco region-keyed) | each agent geolocates its own node IP (mock-geo `/json/<ip>`); providers additionally fetch carbon (mock-eco) keyed by the discovered region. Feeds the eco/latency strategies; the Broker never dials the mocks (§4.7) |
| Broker → any Agent | **never happens** | — | enables unreachable consumer/provider clusters |

### 3.1 Central Cluster

Hosts the **Resource Broker** — a stateless HTTP server in front of a set of CRDs (the source of truth for advertisements, reservations, chunk calculations, and pending instructions). Deployed as a Deployment with leader election. The Broker **never** initiates an outbound connection to an agent; instead, it exposes an HTTPS endpoint that agents call.

### 3.2 Consumer Cluster(s)

Each consumer cluster runs:
- **Cluster Autoscaler (CA)** — vanilla, using the `externalgrpc` cloud provider
- **gRPC Server** — implements the CA's external cloud provider contract (15 gRPC methods); knows only the local Consumer Agent and has no Broker credentials
- **Consumer Agent** — the single point of contact with the Broker on this cluster. Exposes a **local REST API** (consumed in-cluster by the gRPC server) and runs an **outbound-only mTLS client** that polls the Broker every 5 seconds and makes synchronous POSTs for heartbeat, reservation create, and reservation delete. It never listens on any externally reachable port.
- **Liqo** — installed, provides the networking and virtual node infrastructure

### 3.3 Provider Cluster(s)

Each provider cluster runs:
- **Provider Agent** — the single point of contact with the Broker on this cluster. Collects local resource metrics, sends synchronous `POST /api/v1/advertisements` every 30 seconds (CPU/memory/GPU, Liqo cluster ID, topology labels — also acting as heartbeat), and polls the Broker every 5 seconds for pending instructions (`GenerateKubeconfig`, `Cleanup`, `Reconcile`). **Outbound only** — no local listener; no in-cluster client calls it (the gRPC server lives only on consumer clusters).
- **Liqo** — installed, serves as the peering endpoint

---

## 4. Components

### 4.1 Resource Broker (Central Cluster)

**Source:** Extended from `k8s-resource-brokering`.

**Deployment:** Kubernetes Deployment with 2+ replicas and leader election via `coordination.k8s.io/v1` Lease (only the leader serves writes; followers are hot standby). The server itself is stateless — all durable state lives in CRDs.

**Responsibilities:**

| Responsibility | Description |
|---|---|
| Advertisement ingestion | Accepts `POST /api/v1/advertisements` (every 30 s) from provider agents; stores `ClusterAdvertisement` CRDs |
| Consumer registration (implicit) | Learns about consumers via `POST /api/v1/reservations` and `POST /api/v1/heartbeat`; identity comes from the mTLS client certificate CN |
| Chunk calculation | Divides each provider's total resources into equal-sized chunks using the `chunk-config` ConfigMap |
| Provider selection | The provider for a given scale-up is chosen at `GET /api/v1/nodegroups` time, not at reservation time: by default the Broker exposes **all** available providers and the consumer's Cluster Autoscaler grows one (CA's expander is neutral/non-metric). For a consumer that opts in via a `ConsumerPolicy{placement.type: Price\|Eco\|Latency}`, the Broker **masks** its node-group view to expose only the single best provider with capacity for that metric (cheapest / greenest / closest, best-first greedy), so the Broker is the de-facto chooser while CA stays metric-agnostic. See "Metric-based placement" below and §5.6. |
| Reservation commit | `POST /api/v1/reservations` names a specific provider (the one CA grew); the Broker validates its live capacity **synchronously** and returns the reservation record inline |
| Reservation state machine | Manages `Reservation` CRD phases (`Pending` → `GeneratingKubeconfig` → `KubeconfigReady` → `Peering` → `Peered` → `Unpeering` → `Released` \| `Expired` \| `Failed`) |
| Instruction generation | On phase transitions, creates `ProviderInstruction` (for provider agents) and `ReservationInstruction` (for consumer agents) records, filtered per cluster on subsequent `GET /api/v1/instructions` polls |
| Instruction piggyback | The response of `POST /api/v1/advertisements` inlines any pending `ProviderInstruction` records for the calling provider, so typical-case instruction latency is bounded by the 30 s advertisement cadence rather than the 5 s poll — but the 5 s poll guarantees an upper bound |
| Result ingestion | Receives `POST /api/v1/instructions/{id}/result` from agents (kubeconfig for `GenerateKubeconfig`, virtual-node names for `Peer`, tunnel-dropped boolean for `Unpeer`). Marks the instruction `Enforced: true` so it is no longer returned by `GET /instructions`. For kubeconfig payloads the Broker stores the bytes inside the Consumer's pending `ReservationInstruction{Kind: Peer}` so the consumer receives it on its next poll. |
| Rate limiting | Per-cluster token bucket on `GET /api/v1/instructions` — 10 rps burst, 5 rps sustained. Overflow returns `429 TooManyRequests`; the agent's existing exponential backoff absorbs it. |
| Health monitoring | Tracks `lastSeen` on every cluster from advertisements (providers) and heartbeats (consumers). Declares a cluster `Unavailable` after 30 s with no update. |
| Reconcile on drift | On startup and on leader change, issues `ProviderInstruction/ReservationInstruction {Kind: Reconcile}` to every active cluster; agents reply with a full state snapshot via `POST /instructions/{id}/result` and the Broker adopts it. |
| Metric-based placement | Each strategy ranks providers on one advertised metric: **Price** on a per-chunk cost (the Broker multiplies the provider's `unitPrices` — per core-hour / GiB-hour / GPU-hour — by the Broker-owned chunk size), **Eco** on the provider's advertised `carbonIntensity` (lower is greener), **Latency** on the Haversine distance between the consumer's and provider's advertised coordinates (closer is better). The chosen metric drives the per-consumer node-group masking above; Price additionally surfaces the per-chunk `cost` (relayed to the gRPC server's `PricingNodePrice` for the dashboard / an optional CA price expander). Providers missing the relevant metric (no price / no carbon / no coordinates) are a last resort. |

**Metric-based placement (the strictly-additive contract).** All three strategies share one greedy: rank the providers that have the metric, expose the single best one with head-room, mask the rest (`maxSize = currentReserved`), and fall back to metric-less providers only as a last resort. Per-consumer masking only ever *narrows* what the Broker already exposed; it never invents a choice CA didn't have:

| `ConsumerPolicy` | Required metric present? | Node-group view returned | Effective chooser |
|---|---|---|---|
| none | — | all providers (unchanged; any metrics stored + shown but inert) | CA (neutral expander) |
| `type: Price` / `Eco` / `Latency` | no provider has it | all providers (nothing to prefer → no narrowing) | CA (neutral expander) |
| `type: Price` | prices present | only the cheapest priced provider with capacity per chunk type; rest masked; unpriced last | **Broker** |
| `type: Eco` | carbon present | only the lowest-carbon provider with capacity; rest masked; carbon-less last | **Broker** |
| `type: Latency` | consumer **and** provider have coordinates | the top-3 **nearest** providers with capacity exposed as a shortlist; the Consumer Agent UDP-probes them and re-masks to the lowest **measured** RTT; coordinate-less last | **Broker** shortlists, **Consumer Agent** decides |

The required metric for Latency is a **pair**: if the consumer has advertised no coordinates, the Broker applies no preference (all providers stay exposed) — masking can't rank distance from an unknown origin. A consumer pushes its `ConsumerPolicy` (and, for Latency, its coordinates) on every heartbeat (§7.3.3); the Broker stores both per-consumer in the in-memory registry (keyed by the mTLS CN). When the best provider fills up it loses its head-room and the next `/nodegroups` call promotes the next-best (best-first greedy, may span a couple of CA scan intervals).

> **Implemented in:** `cmd/broker/`, `internal/broker/api/` (REST surface + middleware + mTLS; `pricing.go` per-chunk cost, `nodegroups.go` per-consumer masking — `applyMetricPreference` shared greedy with `applyPricePreference` / `applyEcoPreference`, plus the latency-only top-N `applyLatencyTopN` + `consumerProviderDistances`, `geo.go` `haversineKm`, `registry.go` per-consumer policy + coordinates + reported RTTs), `internal/broker/chunk/` (chunk sizing), `internal/controller/broker/` (`clusteradvertisement_controller.go`, `reservation_controller.go`), `internal/controller/autoscaling/` (`providerinstruction_controller.go`, `reservationinstruction_controller.go`, `instruction.go`), `internal/manager/`. The consumer-side measured-latency probing lives in `internal/agent/consumer/latency/` (UDP prober) + `internal/agent/consumer/localapi/` (re-mask to the winner); providers run `ghcr.io/liqotech/udpecho` behind a UDP NodePort (`config/agent/provider/udpecho.yaml`). Deployed via `config/broker/`.

---

### 4.2 gRPC Server (Consumer Cluster)

**Source:** Rebuilt from `Multi-Cluster-Autoscaler`, replacing in-memory state with CRD-based state and **agent-only** external communication.

**Deployment:** Kubernetes Deployment (single replica per consumer cluster, runs alongside CA).

**Important:** The gRPC server has **no Broker client** and **no Broker credentials**. All cross-cluster interaction is the Consumer Agent's responsibility; the gRPC server calls `http://<consumer-agent-service>:<port>/local/*`.

**Responsibilities:**

| Responsibility | Description |
|---|---|
| Implement 15 gRPC methods | Fulfills the `externalgrpc.proto` contract the CA expects |
| Query the Consumer Agent on Refresh | On each `Refresh()` call, issues `GET /local/nodegroups`; the Consumer Agent serves from its local cache (fed by the 5 s Broker poll) |
| Present node groups to CA | Returns one node group per provider cluster / chunk type, with `min=0` and `max=N` where N = available chunks |
| Build node templates | Constructs fake `v1.Node` objects with chunk-sized resources, Liqo labels, and Liqo taints for CA simulation |
| Handle IncreaseSize | Issues `POST /local/reservations`; Consumer Agent returns `202 Accepted` immediately with a `reservationId`. gRPC server then polls `GET /local/reservations/{id}` every 500 ms until `Peered` or CA's deadline. Creates `VirtualNodeState` CRD entries with status `Creating`. |
| Handle DeleteNodes | Issues `DELETE /local/reservations/{id}?chunks=M`; Consumer Agent returns `202`. Marks `VirtualNodeState` entries as `Deleting`. |
| Report pricing | Surfaces the Broker-computed per-chunk cost (relayed by the Consumer Agent) via `PricingNodePrice`. This is informational / for the Broker dashboard — price-based provider selection is done by the **Broker** (per-consumer node-group masking), so the demo runs CA on a neutral expander and does **not** rely on the `price` Expander |
| Cache responses | Caches last known node-group list to serve stale data if the Consumer Agent is temporarily unreachable |

**gRPC Method Implementation Map:**

| gRPC Method | gRPC Server Behavior |
|---|---|
| `NodeGroups` | Returns cached list of node groups (one per provider × chunk type) from latest `Refresh()` |
| `NodeGroupForNode` | Looks up `VirtualNodeState` CRD by node's Liqo cluster ID label to find which group it belongs to |
| `NodeGroupTargetSize` | Returns count of `VirtualNodeState` entries for this group that are not in `Deleting` state |
| `NodeGroupIncreaseSize` | Issues `POST /local/reservations`; polls `GET /local/reservations/{id}` until activated or CA timeout; creates `VirtualNodeState` CRDs |
| `NodeGroupDeleteNodes` | For each node: finds the corresponding `VirtualNodeState`, issues `DELETE /local/reservations/{id}?chunks=1`, marks CRD as `Deleting` |
| `NodeGroupDecreaseTargetSize` | Cancels pending (not-yet-peered) reservations via the Consumer Agent's delete endpoint |
| `NodeGroupNodes` | Lists `VirtualNodeState` CRDs for this group, maps status to `InstanceState` (Creating/Running/Deleting) |
| `NodeGroupTemplateNodeInfo` | If `currentSize >= 1`: reads the actual virtual node's spec. If `currentSize == 0`: constructs a fake node using the Consumer-Agent-provided chunk size, Liqo labels, and Liqo taint |
| `PricingNodePrice` | Returns the Broker-computed per-chunk cost (from the provider's `unitPrices` × chunk size), relayed by the Consumer Agent, scaled by the requested time window. 0 when the provider is unpriced. Informational — CA's expander is neutral, so this does not drive selection |
| `GPULabel` | Returns `"nvidia.com/gpu"` (standard GPU label) |
| `GetAvailableGPUTypes` | Returns GPU types from the Consumer Agent's cached node-group list |
| `Refresh` | Issues `GET /local/nodegroups`; refreshes `VirtualNodeState` statuses via `GET /local/reservations/{id}` for non-terminal reservations |
| `Cleanup` | No-op or cleanup local caches |
| `PricingPodPrice` | Returns a constant `0` — the Broker prices chunks, not pods, and CA's price expander (unused here) treats any constant uniformly |
| `NodeGroupGetOptions` | Returns custom `MaxNodeProvisionDuration` (15 minutes — covers the full `liqoctl peer` handshake including WireGuard tunnel setup and Liqo Identity exchange, which routinely takes 3–5 min on constrained hosts) |

**Node Template Construction (for `NodeGroupTemplateNodeInfo` when `currentSize == 0`):**

```yaml
apiVersion: v1
kind: Node
metadata:
  name: virtual-{provider-cluster-id}-template
  labels:
    type: virtual-node
    liqo.io/remote-cluster-id: "{provider-liqo-cluster-id}"
    node.kubernetes.io/instance-type: "liqo-virtual"
    topology.kubernetes.io/zone: "{provider-zone}"        # from Broker (via Agent) if available
    topology.kubernetes.io/region: "{provider-region}"     # from Broker (via Agent) if available
spec:
  taints:
    - key: virtual-node.liqo.io/not-allowed
      effect: NoExecute
status:
  capacity:
    cpu: "2"              # = standard-cpu chunk size from ConfigMap
    memory: "4Gi"         # = standard-memory chunk size from ConfigMap
    nvidia.com/gpu: "0"   # 0 for standard, chunk gpu-count for GPU groups
    pods: "110"
  allocatable:
    cpu: "2"
    memory: "4Gi"
    nvidia.com/gpu: "0"
    pods: "110"
```

> **Implemented in:** `cmd/grpc-server/`, `internal/grpcserver/` (server + 14 implemented RPCs across `rpc_readonly.go`, `rpc_mutating.go`, `rpc_lifecycle.go`, `nodetemplate.go`), `internal/grpcserver/agentclient/` (typed loopback REST client), `internal/grpcserver/protos/` (vendored externalgrpc.proto **pinned to cluster-autoscaler v1.32.0** — the proto changed shape between master and tagged releases; v1.32+ uses `nodeInfo *v1.Node` on `NodeGroupTemplateNodeInfoResponse` and `*metav1.Time` on pricing request fields, neither of which exists on master. Bump only in lock-step with the CA image tag deployed via `config/grpc-server/`), `internal/controller/autoscaling/virtualnodestate_controller.go`. The node template emitted by `nodetemplate.go` carries `liqo.io/type=virtual-node` and the `virtual-node.liqo.io/not-allowed:NoExecute` taint so CA's NodeAffinity predicate matches workloads that select federation capacity via the documented Liqo pattern. Deployed via `config/grpc-server/`.

---

### 4.3 Consumer Agent (Consumer Cluster)

**Source:** Extended from `k8s-resource-brokering`'s agent, specialized for the consumer role.

**Deployment:** Kubernetes Deployment, **`replicas: 1`, `strategy: Recreate`** (no HA, no leader election — identical to the upstream project). A crashed pod is replaced cleanly; two pods never poll concurrently.

**Network exposure:** **outbound + in-cluster only.** The Consumer Agent opens:
- a **cluster-local listener** on `:8080` (Service type `ClusterIP`) — reachable only by the in-cluster gRPC server;
- an **outbound mTLS HTTP client** targeting the Broker URL.

No Ingress, LoadBalancer, NodePort, or public DNS is required.

**Local API Surface (in-cluster only, consumed by the gRPC server):**

| Method | Endpoint | Description |
|---|---|---|
| GET | `/local/health` | Liveness / readiness |
| GET | `/local/nodegroups` | Served from the Consumer Agent's in-memory cache (populated by the 5 s Broker poll) |
| POST | `/local/reservations` | Returns `202 + reservationId` immediately; internally calls Broker's `POST /api/v1/reservations` synchronously |
| GET | `/local/reservations/{id}` | Returns the cached view of the reservation (phase, virtualNodeNames); refreshed from Broker on cache miss |
| DELETE | `/local/reservations/{id}?chunks=N` | Returns `202`; internally calls Broker's `DELETE /api/v1/reservations/{id}?chunks=N` synchronously |
| GET | `/local/reservations` | Debug / cache warm-up listing |

**Broker-Facing Client (outbound only, mTLS):**

1. Polls `GET /api/v1/instructions` every **5 seconds** (short-poll; same cadence and style as `k8s-resource-brokering`) to pick up `ReservationInstruction` records.
2. Sends synchronous `POST /api/v1/heartbeat` every **15 s** (consumers don't advertise, so heartbeat is a dedicated call).
3. Sends synchronous `POST /api/v1/reservations` and `DELETE /api/v1/reservations/{id}` on demand from the `/local/*` surface.
4. Reports the outcome of each processed instruction via `POST /api/v1/instructions/{id}/result` (idempotent; retries on 5xx / network errors with exponential backoff 1 s → 16 s, max 3 attempts).

**Responsibilities:**

| Responsibility | Description |
|---|---|
| Serve local API | Handle `/local/*` requests from the in-cluster gRPC server |
| Heartbeat | Every 15 s `POST /api/v1/heartbeat` — single liveness signal to the Broker; also carries the consumer's `ConsumerPolicy` placement type (§5.6) |
| Advertise placement inputs | On each heartbeat, **auto-discovers its location**: resolves its own node IP (`NODE_NAME` downward API, or the `--advertised-ip` override) and, when `--mock-geo-url` is configured, geolocates it via mock-geo (`GET /json/<ip>`); sends the discovered `region` / `city` / `latitude` / `longitude` — the consumer's input to the **Latency** strategy. No resolvable IP / no mock-geo ⇒ the consumer opts out of Latency. |
| Forward reservations | Translate `/local/reservations` calls into Broker calls synchronously; maintain a local cache of results for the gRPC server |
| Execute `Peer` | On `ReservationInstruction{Kind: Peer}` (kubeconfig **inlined** in the polling response): store the kubeconfig as a Secret on the consumer cluster, then run `liqoctl peer --gw-server-service-type NodePort --create-resource-slice=false` (the `NodePort` flag is required because Liqo defaults to `LoadBalancer`, which sits at `<pending>` forever on Kind / on-prem clusters without an LB provisioner; `--create-resource-slice=false` because the **agent owns the ResourceSlice** — see below). Then create exactly one `ResourceSlice` named `rs-<reservationID>` in the provider's Liqo tenant namespace, carrying the `liqo.io/replication`, `liqo.io/remoteID` and `liqo.io/remote-cluster-id` labels plus the `liqo.io/create-virtual-node` annotation (without all four Liqo's `VirtualNodeCreatorReconciler` skips it and no node is ever built). `NamespaceOffloading` is **not** created here — it is a per-namespace **singleton** named literally `offloading` (Liqo's `nsoff.validate.liqo.io` webhook hardcodes the name), operator-stamped and shared by every Reservation targeting that namespace. Finally create the `VirtualNodeState` recording the slice name, and POST the result with `resourceSliceNames`. |
| Execute `Unpeer` | On `ReservationInstruction{Kind: Unpeer}`: delete the specified `ResourceSlice`s; if `lastChunk == true`, run full `liqoctl unpeer`; report result |
| Execute `Cleanup` | On `ReservationInstruction{Kind: Cleanup}`: tear down per-Reservation consumer-side artefacts (stale ResourceSlices, orphaned peerings); report result. **NamespaceOffloading is intentionally not deleted** — it is the per-namespace singleton shared with sibling Reservations, so a per-Reservation Cleanup must not touch it. Removal happens out-of-band when the namespace itself is decommissioned (v2 chore). |
| Execute `Reconcile` | On `ReservationInstruction{Kind: Reconcile}`: gather an authoritative local snapshot (active ResourceSlices, VirtualNodeStates, active Liqo peerings) and return it as the `/instructions/{id}/result` payload |
| Idempotency cache | Keyed by `reservationId + instruction kind`, TTL = `idempotency-cache-ttl` (10 min). Duplicate instructions return the cached result instead of re-executing. |
| Local cache population | Merge polling results + synchronous Broker responses into a single in-memory view served to the gRPC server |

> **Implemented in:** `cmd/agent/`, `internal/agent/consumer/` (orchestration), `internal/agent/consumer/heartbeat/` (15 s POST + per-beat node-IP discovery and mock-geo geolocation in `heartbeat.go`), `internal/agent/nodeip/` (node-IP resolver, `--advertised-ip` override), `internal/agent/geo/` (IP-keyed mock-geo client), `internal/agent/consumer/localapi/` (loopback REST), `internal/agent/consumer/instructions/` (Peer / Unpeer / Cleanup / Reconcile + Liqo CR creation + VirtualNodeState management), `internal/agent/client/` (mTLS HTTP client), `internal/agent/poller/` (5 s GET /instructions loop), `internal/agent/health/`. Deployed via `config/agent/consumer/` (Deployment `--role=consumer`).

---

### 4.4 Provider Agent (Provider Cluster)

**Source:** Extended from `k8s-resource-brokering`'s agent, specialized for the provider role.

**Deployment:** Kubernetes Deployment, **`replicas: 1`, `strategy: Recreate`** (no HA, no leader election — identical to the upstream project). A crashed pod is replaced cleanly; two pods never poll concurrently.

**Network exposure:** **outbound only.** The Provider Agent opens:
- an **outbound mTLS HTTP client** targeting the Broker URL.

There is **no local HTTP listener** — the gRPC server runs only on consumer clusters, and nothing inside the provider cluster calls the agent directly. No Ingress, LoadBalancer, NodePort, or public DNS is required.

**Broker-Facing Client (outbound only, mTLS):**

1. Sends synchronous `POST /api/v1/advertisements` every **30 seconds** with the latest capacity snapshot; the Broker's response piggybacks any `ProviderInstruction` records ready for this cluster, reducing the common-case instruction latency to the 30 s advertisement cadence.
2. Polls `GET /api/v1/instructions` every **5 seconds** — upper-bound fallback path when piggyback is empty or a new instruction arrives between advertisement ticks.
3. Reports the outcome of each processed instruction via `POST /api/v1/instructions/{id}/result` (idempotent; retries on 5xx / network errors with exponential backoff 1 s → 16 s, max 3 attempts).

Advertisement doubles as heartbeat — providers never call `/api/v1/heartbeat`.

**Responsibilities:**

| Responsibility | Description |
|---|---|
| Collect metrics | Reads local node resources (allocatable − used), GPU availability, topology labels (zone/region) |
| Advertise capacity | Every 30 s `POST /api/v1/advertisements` — also acts as heartbeat; response carries piggybacked `ProviderInstruction`s |
| Advertise prices (optional) | Reads its per-resource `unitPrices` from a mounted file (`--price-file`) on **every** advertisement cycle, so an operator can reprice without a restart, and includes them in the POST. A missing/empty/unparseable file ⇒ no price advertised (the provider is simply not eligible for price-preferring consumers) |
| Advertise capacity % (optional) | Reads a per-resource percentage of allocatable to donate from `--capacity-file` (`agent-capacity` ConfigMap) every cycle and sends it as `capacityScalePercent`; the Broker scales the advertised allocatable before chunking. A value in (0,100) donates that fraction; 100 / >100 / ≤0 / unset ⇒ full allocatable. Lets a provider hold back capacity without a restart |
| Advertise location, carbon & coordinates (optional) | **Auto-discovers its location** every cycle: resolves its node IP (`NODE_NAME`, or `--advertised-ip`) and geolocates it via mock-geo (`GET /json/<ip>`, `--mock-geo-url`) → `topology.region` / `topology.city` / `topology.latitude/longitude` (the **Latency** input). The discovered `region` code then keys the carbon lookup from mock-eco (`GET /carbon/forecast?region=` with `/carbon?region=` fallback, `--mock-eco-url`) → `carbonIntensity` + `carbonForecast` (the **Eco** input). Every fetch is best-effort: a failure omits that field and never fails the publish. No resolvable location ⇒ the provider opts out of Eco/Latency |
| Execute `GenerateKubeconfig` | On receipt of a `ProviderInstruction{Kind: GenerateKubeconfig}` (via poll or piggyback): runs `liqoctl generate peering-user --consumer-cluster-id <id>`; POSTs the resulting kubeconfig as payload of `POST /api/v1/instructions/{id}/result`. **Per-consumer singleton invariant** — `liqoctl generate peering-user` produces one `liqo-peer-user-<consumer>` identity per consumer, not per Reservation; if N Reservations from the same consumer hit the same provider, only the first GK call succeeds and the others return `CSR already exists`. Failed GKs are propagated up; the broker-side fix (v2: de-duplicate GK across (consumer, provider) pairs and reuse the cached kubeconfig) is tracked separately. Handler intentionally does **not** self-heal by re-running `liqoctl delete peering-user` — every regeneration mints a fresh random CN that invalidates any kubeconfig the broker has already captured for the surviving Reservation. |
| Execute `Cleanup` | On `ProviderInstruction{Kind: Cleanup}`: tears down provider-side artefacts (peering-user kubeconfig, associated state); reports result |
| Execute `Reconcile` | On `ProviderInstruction{Kind: Reconcile}`: gathers current live advertisement state and active-reservation state; returns it in `POST /instructions/{id}/result` |
| Idempotency cache | Keyed by `reservationId + instruction kind`, TTL = `idempotency-cache-ttl` (10 min). Duplicate instructions return the cached result instead of re-executing. |

> **Shared code, separate binaries.** Both agents share the outbound mTLS client, the exponential-backoff retrier, the idempotency cache, and the CRD clients. Role is selected at startup by a CLI flag (`--role=consumer|provider`) that wires in the role-specific instruction executors and the role-specific polling / advertisement loop.

> **Implemented in:** `cmd/agent/`, `internal/agent/provider/` (orchestration), `internal/agent/provider/snapshot/` (allocatable + topology), `internal/agent/provider/advertise/` (30 s POST + per-cycle `--price-file` / `--capacity-file` reads and node-IP discovery + mock-eco/mock-geo fetch in `publisher.go` — `loadUnitPrices`, `loadCapacityPercents`, `loadPlacementInputs`, `loadCarbon`), `internal/agent/nodeip/` (node-IP resolver), `internal/agent/eco/` (region-keyed carbon client) + `internal/agent/geo/` (IP-keyed geo client), `internal/agent/provider/instructions/` (GenerateKubeconfig via `liqoctl generate peering-user` + Cleanup + Reconcile), `internal/agent/client/`, `internal/agent/poller/`, `internal/agent/health/`. Deployed via `config/agent/provider/` (Deployment `--role=provider`). `liqoctl` is bundled in the agent image — the **host** prefetches `bin/liqoctl-<version>-<os>-<arch>/liqoctl` via the `Makefile`'s `LIQOCTL_BIN` target and the `Dockerfile` `COPY`s it from the build context (not `curl`-ed from inside the build, because some hosts blackhole the docker daemon's egress to `github.com/releases`). Bump `LIQOCTL_VERSION` in the `Makefile` in lock-step with the Liqo CRDs the consumer / provider instruction handlers create.

---

### 4.5 Cluster Autoscaler (Consumer Cluster)

**Source:** Upstream Kubernetes Cluster Autoscaler, unmodified.

**Configuration:**
```
--cloud-provider=externalgrpc
--cloud-config=/etc/autoscaler/cloud-config
--expander=least-waste                 # neutral / NON-price (see note)
--scale-down-enabled=true
--scale-down-unneeded-time=5m
--max-node-provision-time=5m
--scan-interval=10s
```

**Expander is deliberately NOT `price`.** Price-based provider selection is the Broker's job: it masks `GET /api/v1/nodegroups` per consumer that opts in via a `ConsumerPolicy` (§4.1, §5.6), so CA only ever sees the provider(s) the Broker wants it to grow. A `price` expander here would make CA price-select for **every** consumer using `PricingNodePrice`, regardless of policy — defeating the per-consumer opt-in. CA therefore stays completely price-agnostic; `least-waste` (or any non-price expander) is fine because, under masking, only the chosen provider has head-room anyway.

**Cloud config file:**
```
[Global]
address=localhost:50051
```

**Key point:** The CA is completely unaware of Liqo, the Broker, the Consumer Agent, or multi-provider topology. It sees node groups and makes decisions using its standard algorithms. The gRPC server acts as the translation layer.

> **Implemented in:** upstream `cluster-autoscaler` (see `../cluster-autoscaler/`); deployment template + cert-manager client cert + cloud-config Secret rendered by `test/e2e/bootstrap/cluster_autoscaler.go` (`registry.k8s.io/autoscaling/cluster-autoscaler:v1.32.0`).

---

### 4.6 Liqo (Consumer and Provider Clusters)

**Source:** Upstream Liqo, unmodified.

**Role:** Provides multi-cluster networking, authentication, and virtual node infrastructure.

**Key resources used:**
- `ResourceSlice` — Created by the consumer agent to request specific resources from a peered provider. Each ResourceSlice represents one chunk.
- `VirtualNode` — Automatically created by Liqo when a ResourceSlice is accepted. This is what the CA sees as a regular node.
- `NamespaceOffloading` — Created by the consumer agent to enable pod scheduling on virtual nodes for specific namespaces.

**Peering mode:** On-demand — established only when the CA first requests scale-up on a given provider, torn down when the last chunk is released.

> **Implemented in:** upstream Liqo (see `../liqo/`). Provider Agent shells out to `liqoctl generate peering-user` (`internal/agent/provider/instructions/generatekubeconfig.go`); Consumer Agent shells out to `liqoctl peer --gw-server-service-type NodePort` / `liqoctl unpeer` and creates the `ResourceSlice` via `unstructured.Unstructured` (`internal/agent/consumer/instructions/liqo.go`, `peer.go`); on `Unpeer` it deletes the `ResourceSlice` and, on `LastChunk`, the leftover `ForeignCluster` shell `liqoctl unpeer` leaves behind (`unpeer.go`). The singleton `NamespaceOffloading` named literally `offloading` is **operator-stamped** (the Ansible `fa_consumer` role / bootstrap) — the agent never creates or deletes it. The materialised `v1.Node` is read by `internal/controller/autoscaling/virtualnodestate_controller.go`. `liqoctl` is pinned to v1.1.2 (`LIQOCTL_VERSION` in the `Makefile`), prefetched on the host into `bin/liqoctl-*/liqoctl`, and `COPY`-ed into the agent image at build time.

---

### 4.7 Mock Services (Mock Cluster)

Two tiny in-repo HTTP services that stand in for real external APIs and feed the **Eco** and **Latency** strategies with carbon + location data. They exist so the placement strategies are demonstrable without contracting a live carbon-intensity or geo-IP provider, while preserving the **broker-dial-out-free** invariant: only **agents** fetch from the mocks (during their normal advertisement / heartbeat cycle), and they advertise the resulting numbers to the Broker — the Broker never calls the mocks.

| Service | Endpoint | Returns | Consumed by |
|---|---|---|---|
| `mock-eco` | `GET /carbon?region=<code>` (+ `GET /carbon/forecast?region=`) | `{"region", "carbonIntensity"}` — the region's current-hour value (the forecast endpoint returns the next 24 h) from a built-in 24-hour gCO2eq/kWh profile | Provider Agent (Eco) |
| `mock-geo` | `GET /json/<ip>` | an ip-api.com-style location `{"query","status","region","regionName","city","countryCode","continentCode","lat","lon","isp","org","as"}` resolved by **longest-prefix CIDR match** | Provider **and** Consumer Agents (Latency + auto-location) |

**mock-geo is IP-keyed** (a real geo-IP service works by prefix, not by hashing or by an operator-chosen region code): each agent reads its own node IP and looks it up, and the returned `region` code is what keys the **region-keyed** mock-eco carbon lookup — so one discovered location drives both strategies. Both are unauthenticated plain HTTP and also serve `GET /healthz`; mock-eco returns `404` for an unknown region, mock-geo returns an ip-api `"fail"` envelope for an unparseable IP and a no-location default row for an IP outside every known subnet. They run on a **dedicated mock cluster** (the demo's "option A" topology — a separate single-node VM) so they do not perturb the federation clusters; agents reach them via `--mock-eco-url` / `--mock-geo-url` (provider) and `--mock-geo-url` (consumer), wired by the deploy. Omitting the URLs disables the lookups and the eco/latency strategies stay inert.

> **Implemented in:** `cmd/mock-eco/`, `cmd/mock-geo/`, `internal/mockeco/server.go` (region → 24 h carbon profile), `internal/mockgeo/server.go` (CIDR → real-city table + longest-prefix `/json/{ip}` handler; the demo's private `/24`s are carved into `/26` blocks so co-located clusters land in distinct cities). Deployed via `config/mock-eco/`, `config/mock-geo/` and the Ansible `fa_mocks` role; `deploy/ansible/scripts/demo-up.sh --mocks <ip>` adds the mock cluster and auto-wires the URLs into every agent's `agent-config`.

---

### 4.8 Operator Dashboards

Three browser UIs surface and drive the system. None is on the agents' critical path; all are plain HTTP.

**(a) Broker dashboard — read-only.** The Broker serves a self-contained single-page dashboard on a **separate plain-HTTP listener** (default `:9444`; the Ansible `fa_central` role flips the Service to NodePort `30444`), distinct from the mTLS API. It polls `GET /api/v1/overview` — a live projection of the four Broker CRDs plus the in-memory consumer registry: provider advertisements (with per-chunk **cost**, **carbon**, **region**), reservations, the instruction phase machine, federation chunk capacity, and registered consumers (with their placement policy). It is leader-elected (served by the active Broker only), unauthenticated, and read-only (every cell rendered via `textContent`).

> **Implemented in:** `internal/broker/api/dashboard.go` (`/` SPA via `//go:embed dashboard_assets/index.html`, `GET /api/v1/overview`, auth-free middleware chain), `internal/broker/api/dashboard_runnable.go` (plain-HTTP `manager.Runnable`). Exposed via the `dashboard` port in `config/broker/service.yaml` (NodePort `30444` applied by `deploy/ansible` `fa_central`).

**(b) Liqo dashboard — third-party.** An upstream [liqo-dashboard](https://github.com/ArubaKube/liqo-dashboard) deployed on the consumer cluster by the Ansible install, served via the cluster's Traefik Ingress on host `liqo-dashboard.local`. It visualises Liqo state — peerings, virtual nodes, offloaded pods — complementing the federation-autoscaler view.

> **Implemented in:** deployed by `deploy/ansible` (`02-deploy`, consumer role) — not part of the Go codebase.

**(c) Agent config consoles — read/write.** Each consumer and provider agent serves a small **role-aware** config console on a NodePort (`30445`) so an operator can set that cluster's federation knobs from a browser instead of `kubectl apply`-ing YAML. The **consumer** console sets the placement policy (Standard / Price / Eco / Latency) and applies/deletes the demo workload; the **provider** console sets unit prices, advertised CPU/RAM capacity %, and the renewable flag. Both also show each cluster's **auto-discovered location** read-only (location is geolocated from the node IP, not operator-set). It writes the same resources the samples do — `ConsumerPolicy`, the `agent-prices` / `agent-capacity` / `agent-renewable` ConfigMaps, and the workload Deployment — so changes take effect on the next heartbeat (~15 s) / advertisement (~30 s); each header shows the cluster's federation ID + Liqo ID. It is **plain-HTTP, unauthenticated, and write-capable** — demo-grade, intended for a trusted/management network — and is a **separate listener** from the consumer's loopback `localapi` (which stays loopback-only as the gRPC server's trust boundary). Enabled via `--console-bind-address` (empty ⇒ disabled; overlays set `:9095`).

> **Implemented in:** `internal/agent/console/` (`server.go` role-gated routes + ConfigMap upsert, `state.go` current-state projection, `workload.go` embedded burst workload, `assets/{consumer,provider}.html`), started from `internal/agent/{consumer,provider}/`. Exposed via `config/agent/{consumer,provider}/console-service.yaml` (NodePort `30445`); write RBAC in `config/agent/base/clusterrole.yaml` (configmaps + consumerpolicies + nodes-read) and `config/agent/consumer/workload_role.yaml` (the `default`-namespace Deployment). The read-only discovered-location card reuses `internal/agent/nodeip/` + `internal/agent/geo/`.

---

## 5. Custom Resource Definitions (CRDs)

All CRDs remain at `v1alpha1`; changes versus `k8s-resource-brokering` are **additive only** (new fields, no renames, no breaking type changes).

### 5.1 VirtualNodeState (Consumer Cluster, managed by gRPC server)

Tracks the lifecycle of each virtual-node chunk on the consumer cluster.

```yaml
apiVersion: autoscaling.federation.io/v1alpha1
kind: VirtualNodeState
metadata:
  name: provider1-chunk-0
  namespace: federation-system
  labels:
    federation.io/provider-cluster-id: "provider-1"
    federation.io/node-group-id: "ng-provider-1-standard"
    federation.io/chunk-type: "standard"
spec:
  providerClusterId:     "provider-1"
  providerLiqoClusterId: "liqo-provider-1-xxxx"
  nodeGroupId:           "ng-provider-1-standard"
  chunkIndex:            0
  reservationId:         "res-abc123"
  resources:
    cpu:            "2"
    memory:         "4Gi"
    nvidia.com/gpu: "0"
status:
  phase:              Creating    # Creating | Running | Deleting | Failed
  virtualNodeName:    ""          # populated when Liqo creates the virtual node
  resourceSliceName:  ""          # populated when agent creates the ResourceSlice
  lastTransitionTime: "2026-04-08T10:30:00Z"
  message:            ""
```

Phase transitions:
```
Creating → Running → Deleting → (deleted)
Creating → Failed → (cleaned up)
```

> **Implemented in:** `api/autoscaling/v1alpha1/virtualnodestate_types.go` (CRD), `internal/controller/autoscaling/virtualnodestate_controller.go` (reconciler projecting the cluster-scoped `v1.Node`'s Ready / allocatable / `providerID` onto status — correlated by the node named after the provider's Liqo cluster ID, **not** the Liqo `VirtualNode` CR), `internal/agent/consumer/instructions/virtualnodestate.go` (Peer creates / Unpeer deletes the CR).

### 5.2 ClusterAdvertisement (Broker)

From `k8s-resource-brokering`, extended with Liqo and chunk fields. **No `agentEndpoint` field** — the Broker never dials the agent.

```yaml
apiVersion: brokering.federation.io/v1alpha1
kind: ClusterAdvertisement
metadata:
  name: provider-1
  namespace: federation-system
spec:
  clusterId:      "provider-1"
  liqoClusterId:  "liqo-provider-1-xxxx"
  clusterType:    "standard"          # standard | gpu
  resources:
    allocatable:
      cpu:            "8"
      memory:         "16Gi"
      nvidia.com/gpu: "0"
  topology:
    zone:      "qc-a"
    region:    "QC"                    # auto-discovered; also the region key for the mock-eco carbon lookup
    city:      "Montreal"              # auto-discovered; informational (dashboard display)
    latitude:  45.6085                 # optional; decision-engine input only (Latency), NOT surfaced as a node label
    longitude: -73.5493
  unitPrices:                         # optional, per-resource price-per-unit-per-hour
    cpu:            "0.03"             #   per core-hour
    memory:        "0.004"            #   per GiB-hour
    nvidia.com/gpu: "1.50"            #   per GPU-hour
                                      # Broker derives a per-chunk cost = unitPrices × chunk size.
                                      # Omitted ⇒ unpriced (last resort for price-preferring consumers).
  carbonIntensity: 25.0               # optional gCO2eq/kWh for this region (Eco input); omitted ⇒ not eco-rankable
status:
  lastSeen:        "2026-04-08T10:30:00Z"
  available:       true               # false if heartbeat timeout exceeded
  totalChunks:     4
  reservedChunks:  1
  availableChunks: 3
```

> **Implemented in:** `api/broker/v1alpha1/clusteradvertisement_types.go` (CRD — incl. `CarbonIntensity *float64` / `CarbonForecast []float64`) and `api/broker/v1alpha1/common_types.go` (`Topology.Region` / `Topology.City` / `Topology.Latitude` / `Topology.Longitude`), `internal/controller/broker/clusteradvertisement_controller.go` (`StaleAfter` freshness check flipping `Available`), `internal/broker/api/advertisement.go` (`POST /api/v1/advertisements` handler + chunk decoration; maps carbon/topology onto the CR).

### 5.3 Reservation (Broker)

From `k8s-resource-brokering`, extended with chunk tracking, Liqo IDs and new phases.

```yaml
apiVersion: brokering.federation.io/v1alpha1
kind: Reservation
metadata:
  name: res-abc123
  namespace: federation-system
spec:
  consumerClusterId:     "consumer-1"
  consumerLiqoClusterId: "liqo-consumer-1-xxxx"
  providerClusterId:     "provider-1"
  providerLiqoClusterId: "liqo-provider-1-xxxx"
  chunkCount:            1
  chunkType:             "standard"   # standard | gpu
  resources:
    cpu:            "2"
    memory:         "4Gi"
    nvidia.com/gpu: "0"
status:
  phase:     Pending      # Pending | GeneratingKubeconfig | KubeconfigReady | Peering | Peered | Unpeering | Released | Expired | Failed
  createdAt: "2026-04-08T10:30:00Z"
  expiresAt: "2026-04-08T10:35:00Z"   # createdAt + reservation-timeout
  virtualNodeNames:
    - "liqo-node-provider-1-0"
  message: ""
```

Phase transitions:
```
Pending → GeneratingKubeconfig → KubeconfigReady → Peering → Peered
Peered → Unpeering → Released
{Pending, GeneratingKubeconfig, KubeconfigReady, Peering} → Failed | Expired
```

> **Implemented in:** `api/broker/v1alpha1/reservation_types.go` (CRD, incl. `Status.TerminatedAt` — the terminal-GC clock), `internal/controller/broker/reservation_controller.go` (phase machine + expiry guard + `checkProviderAvailable` + terminal GC via `DefaultTerminalReservationTTL`), `internal/broker/api/reservation.go` (`POST /api/v1/reservations` synchronous decision engine + `DELETE` handler). v1 limitation: partial release deferred — the `LastChunk: true` hardcode lives in `reservation_controller.go`'s `handleUnpeering`.

### 5.4 ProviderInstruction (Provider Agent cluster)

From `k8s-resource-brokering`, extended with `kind` and chunk fields. Created by the Broker; polled by the provider agent via `GET /api/v1/instructions`.

```yaml
apiVersion: agent.federation.io/v1alpha1
kind: ProviderInstruction
metadata:
  name: inst-res-abc123-genkube
spec:
  reservationId:         "res-abc123"
  kind:                  "GenerateKubeconfig"  # GenerateKubeconfig | Cleanup | Reconcile
  consumerClusterId:     "consumer-1"
  consumerLiqoClusterId: "liqo-consumer-1-xxxx"
  chunkCount:            1
  lastChunk:             false
  expiresAt:             "2026-04-08T10:35:00Z"
status:
  enforced:       false
  lastUpdateTime: "2026-04-08T10:30:00Z"
```

> **Implemented in:** `api/autoscaling/v1alpha1/providerinstruction_types.go` (CRD), `internal/controller/autoscaling/providerinstruction_controller.go` (delivery + enforcement), `internal/controller/autoscaling/instruction.go` (shared `IssuedAt` + `DefaultEnforcedTTL` bookkeeping); emitted by `internal/controller/broker/reservation_controller.go` on phase transitions.

### 5.5 ReservationInstruction (Consumer Agent cluster)

From `k8s-resource-brokering`, extended. Created by the Broker; polled by the consumer agent via `GET /api/v1/instructions`. `Peer` instructions carry the kubeconfig **inlined** in the JSON returned by the polling endpoint (never stored in etcd on the consumer side).

```yaml
apiVersion: agent.federation.io/v1alpha1
kind: ReservationInstruction
metadata:
  name: inst-res-abc123-peer
spec:
  reservationId:         "res-abc123"
  kind:                  "Peer"   # Peer | Unpeer | Cleanup | Reconcile
  providerClusterId:     "provider-1"
  providerLiqoClusterId: "liqo-provider-1-xxxx"
  resourceSliceNames:    []
  chunkCount:            1
  lastChunk:             false
  kubeconfigRef:         "secret-name-on-broker-cluster"  # internal; payload is inlined in the polling response
  expiresAt:             "2026-04-08T10:35:00Z"
status:
  delivered:      false
  lastUpdateTime: "2026-04-08T10:30:00Z"
```

> **Implemented in:** `api/autoscaling/v1alpha1/reservationinstruction_types.go` (CRD), `internal/controller/autoscaling/reservationinstruction_controller.go` (delivery + enforcement), `internal/controller/autoscaling/instruction.go`. Kubeconfig payload is loaded fresh per poll by `internal/broker/api/instructions.go` (never written to status).

### 5.6 ConsumerPolicy (Consumer Cluster, operator-stamped)

A consumer-cluster-local declaration of how the Broker should place this consumer's borrowed capacity. It lives **only on the consumer cluster** and the Broker never reads it directly — the Consumer Agent reads it on every heartbeat and pushes its spec to the Broker (preserving the agent-initiated, no-Broker-dial-in model). Operator-stamped (manually, by the `fa_consumer` Ansible role, or via the consumer config console §4.8) and editable at any time; the agent re-reads it each heartbeat so a change takes effect within ~15 s without a restart. The type is a discriminated union of three single-metric strategies: **Price** (cheapest), **Eco** (lowest carbon), **Latency** (closest).

```yaml
apiVersion: autoscaling.federation-autoscaler.io/v1alpha1
kind: ConsumerPolicy
metadata:
  name: default
  namespace: federation-autoscaler-system
spec:
  placement:
    type: Price        # Price | Eco | Latency; "" (or no ConsumerPolicy) = Broker default, no preference
```

The `type` is validated by a kubebuilder enum (`Price;Eco;Latency`). When set, the Broker narrows this consumer's `GET /api/v1/nodegroups` view for that metric — to the single cheapest (**Price**) or greenest (**Eco**) provider with capacity, or (for **Latency**) to the top-3 **nearest** by Haversine distance, which the Consumer Agent then UDP-probes and re-masks to the lowest measured RTT — per §4.1, "Metric-based placement". No `ConsumerPolicy`, or no provider carrying the relevant metric (and, for Latency, no consumer coordinates), leaves the view unchanged, so the feature is strictly additive.

> **Implemented in:** `api/autoscaling/v1alpha1/consumerpolicy_types.go` (CRD + `PlacementPolicy`). Consumer Agent reads it each heartbeat in `internal/agent/consumer/heartbeat/heartbeat.go` (`currentPlacement`) and sends it on `HeartbeatRequest.Placement`; the Broker stores it per-consumer in `internal/broker/api/registry.go` and consumes it in `internal/broker/api/nodegroups.go`. RBAC: read-only in `config/agent/base/clusterrole.yaml`.

---

## 6. ConfigMap Definition

Deployed on the **central cluster**, in the Broker's namespace. Agents do not read this ConfigMap — they learn the effective chunk size per reservation from the `Reservation` body they receive.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: chunk-config
  namespace: federation-system
data:
  # Standard (non-GPU) chunk size
  standard-cpu: "2"
  standard-memory: "4Gi"

  # GPU chunk size
  gpu-cpu: "4"
  gpu-memory: "8Gi"
  gpu-count: "1"

  # Timeouts. reservation-timeout must comfortably exceed a full
  # `liqoctl peer` invocation (gateway pod start + WireGuard handshake +
  # Identity exchange) — that's 3–5 min on a healthy host, longer on
  # constrained CI VMs. 15m gives headroom; the broker hard-codes this
  # default in internal/broker/api/reservation.go and the gRPC server
  # mirrors it via MaxNodeProvisionDuration in NodeGroupGetOptions.
  reservation-timeout: "15m"
  agent-heartbeat-timeout: "30s"

  # Polling + rate limiting
  agent-poll-interval: "5s"
  agent-heartbeat-interval: "15s"
  provider-advertisement-interval: "30s"
  instructions-rate-limit-burst: "10"
  instructions-rate-limit-sustained: "5"

  # Idempotency / retry
  idempotency-cache-ttl: "10m"
  agent-request-timeout: "30s"
  agent-request-retries: "3"
```

**Rules:**
- A provider is classified as **GPU** if it advertises `nvidia.com/gpu > 0`, otherwise **standard**
- For standard providers: `chunks = min(cpu / standard-cpu, memory / standard-memory)` (floor, discard leftover)
- For GPU providers: `chunks = min(cpu / gpu-cpu, memory / gpu-memory, gpu / gpu-count)` (floor, discard leftover)
- Timeouts are aligned: heartbeat timeout `30s` = 2 × heartbeat interval `15s`, giving one missed beat of tolerance
- `idempotency-cache-ttl` bounds the age of retriable instructions

> **Implemented in:** `internal/broker/chunk/` (chunk-sizing math + `DefaultSizer` fallback constants); sample ConfigMap shipped at `config/samples/chunk-config.yaml`. The broker consumes the ConfigMap on every `/api/v1/reservations` call via `internal/broker/api/reservation.go`.

**Per-cluster agent ConfigMaps (consumer / provider).** Separately from the central `chunk-config` above, each agent reads a few small ConfigMaps **mounted as optional volumes in its own namespace** and re-read every cycle, so an operator can change them live without a restart. These are exactly what the config consoles (§4.8) and the Ansible samples write:

| ConfigMap | Key (shape) | Read by | Drives |
|---|---|---|---|
| `agent-prices` | `prices.yaml` → `cpu` / `memory` unit prices | provider (`--price-file`) | `unitPrices` → Price |
| `agent-capacity` | `capacity.yaml` → `cpu` / `memory` % or fixed quantity | provider (`--capacity-file`) | `capacityScalePercent` / `capacityFixed` → donated fraction of allocatable |
| `agent-renewable` | `renewable.yaml` → `renewable: <bool>` | provider (`--renewable-file`) | `renewable` flag → the Standard composite bonus |

Location is **not** a ConfigMap — it is auto-discovered from the node IP (§4.7). The mock-service base URLs and the optional `--advertised-ip` override are deploy-time infrastructure, injected via the `agent-config` ConfigMap (`mockEcoUrl`, `mockGeoUrl`, `advertisedIp`) → `--mock-eco-url` / `--mock-geo-url` / `--advertised-ip`; the node name arrives via the `NODE_NAME` downward-API env.

> **Implemented in:** read by `internal/agent/provider/advertise/publisher.go` (`loadUnitPrices` / `loadCapacityPercents` / `loadRenewable`). ConfigMap bases + volume mounts in `config/agent/provider/{prices,capacity,renewable}-configmap.yaml`; samples in `deploy/ansible/samples/{*-prices,provider-capacity}.yaml`. The provider console (§4.8) upserts these same ConfigMaps. Location is auto-discovered (§4.7), not a ConfigMap.

---

## 7. API Contracts

This section defines **every** wire-level interface used in the system. Three surfaces exist (no Broker → Agent surface — the Broker never initiates an outbound call):

| # | Surface | Transport | Initiator | Auth |
|---|---|---|---|---|
| 7.1 | gRPC Server ↔ Cluster Autoscaler | gRPC (HTTP/2) | CA | upstream TLS (in-cluster) |
| 7.2 | gRPC Server ↔ Consumer Agent | HTTP/REST (in-cluster) | gRPC Server | optional ServiceAccount token |
| 7.3 | Consumer / Provider Agent → Broker | HTTPS/REST (cross-cluster) | **Agent only** | mTLS |

### 7.0 Common Conventions

**API version.** REST endpoints are prefixed with `/api/v1/` (cross-cluster) or `/local/` (in-cluster). Breaking changes go to `/api/v2/`; the previous version is supported for one minor release.

**Content type.** `application/json; charset=utf-8` for both request and response bodies.

**Cluster identity.** The Broker extracts `clusterId` from the `CN=…` of the agent's mTLS client certificate. Payload fields like `clusterId` are informational and cross-checked against the cert; mismatch → `403 Forbidden`. Every agent's `clusterId` equals its Liqo cluster ID, so one identity is used throughout.

**Idempotency.** Every mutating call carries `X-Reservation-Id` (reservation-scoped calls) or `X-Request-Id` (other calls). The receiver caches the response for `idempotency-cache-ttl` (10 min) and replays it verbatim for any retry with the same ID.

**Correlation.** Clients SHOULD send `X-Request-Id: <uuid>`; servers echo it in the response and in their logs.

**Standard error response.**
```
HTTP/1.1 <4xx|5xx>
Content-Type: application/json

{
  "code":      "ReservationExpired",
  "message":   "Reservation res-abc123 expired at 2026-04-08T10:35:00Z",
  "details":   { "reservationId": "res-abc123", "expiredAt": "2026-04-08T10:35:00Z" },
  "requestId": "c1f0...-e4a2"
}
```

**Stable error codes:**

| HTTP | `code`                 | Meaning |
|------|------------------------|---------|
| 400  | `InvalidRequest`       | Malformed payload or missing required field |
| 401  | `Unauthenticated`      | mTLS cert invalid / SA token missing |
| 403  | `Forbidden`            | Cert does not authorize this clusterId |
| 404  | `NotFound`             | Reservation / NodeGroup / Consumer not found |
| 409  | `Conflict`             | Idempotency mismatch or reservation in an incompatible phase |
| 410  | `ReservationExpired`   | Reservation already expired (default >15m, configurable via `reservation-timeout` in the `chunk-config` ConfigMap) |
| 422  | `InsufficientCapacity` | Not enough free chunks on the requested provider |
| 429  | `TooManyRequests`      | Rate-limited |
| 500  | `InternalError`        | Unhandled server-side failure |
| 502  | `UpstreamError`        | A dependency (Liqo, Kubernetes API) failed |
| 503  | `ServiceUnavailable`   | Broker unavailable or leader election in flight |
| 504  | `Timeout`              | Single-call timeout exceeded |

**Timeouts & retries (agent is always the retrier; Broker never retries because it never initiates):**
- Agent → Broker call timeout: **30 s** per attempt, **3 attempts**, exponential backoff (1 s, 4 s, 16 s, ±20 % jitter).

> **Implemented in:** `internal/broker/api/types.go` (`ErrCode*` constants, `HeaderRequestID`, `HeaderReservationID`), `internal/broker/api/middleware.go` (`ClusterIDMiddleware` deriving identity from the cert CN + `RequestIDMiddleware` + token-bucket rate limiter + structured-logging wrapper), `internal/broker/api/tls.go` (mTLS server config), `internal/agent/client/` (`request.go` retry+backoff, `errors.go` typed `Error` categories, `tls.go` client-side mTLS config).

---

### 7.1 gRPC Server ↔ Cluster Autoscaler

Defined by upstream `externalgrpc.proto`. The gRPC server implements all 15 methods listed in § 4.2 with no non-upstream extensions.

The vendored proto is pinned to **cluster-autoscaler v1.32.0**. The proto's message shapes changed between unreleased master and tagged 1.32.x releases (notably: `NodeGroupTemplateNodeInfoResponse.nodeInfo` is a structured `v1.Node` on v1.32+ but was `bytes nodeBytes` on master; `PricingNodePriceRequest.{StartTime,EndTime}` are `*metav1.Time` on v1.32+ but `timestamppb.Timestamp` on master). Bump the vendored proto only in lock-step with the CA image tag deployed by `test/e2e/bootstrap/cluster_autoscaler.go` (`ClusterAutoscalerImage`, e2e) / `deploy/ansible/roles/cluster_autoscaler/` (`cluster_autoscaler_image`, demo); running mismatched versions surfaces as a `nil pointer` in CA when it tries to read `NodeInfo` against the wrong wire layout.

> **Implemented in:** `internal/grpcserver/protos/` (vendored proto bindings — `externalgrpc.pb.go`, `externalgrpc_grpc.pb.go` — pinned to v1.32.0); `internal/grpcserver/server.go` builds the gRPC server with mTLS, and the 14 implemented RPCs land in `rpc_readonly.go`, `rpc_mutating.go`, `rpc_lifecycle.go`. `NodeGroupGetOptions` is the only Unimplemented RPC (per proto contract).

---

### 7.2 gRPC Server ↔ Consumer Agent  (REST, in-cluster)

Plain HTTP over a ClusterIP Service, e.g. `http://federation-consumer-agent.federation-system.svc.cluster.local:8080`. Optional `Authorization: Bearer <ServiceAccount token>`. The provider-side cluster has no equivalent surface — this endpoint family exists only on the consumer side.

> **Implemented in:** `internal/agent/consumer/localapi/` (server + per-route handlers + `VirtualNodeView` projection over `VirtualNodeState` CRs); `internal/grpcserver/agentclient/` is the typed client the gRPC server dials with. v1 implements `/local/nodegroups`, `/local/reservations`, `DELETE /local/reservations/{id}`, `/local/virtual-nodes`, and `/healthz`. The single-reservation `GET /local/reservations/{id}` (§7.2.4) and listing (§7.2.6) are not yet implemented — the gRPC server polls broker state directly.

#### 7.2.1 `GET /local/health`
```
GET /local/health

Response 200:
{"status": "healthy", "brokerReachable": true, "lastPollAgoSeconds": 3, "uptimeSeconds": 12345}
```

#### 7.2.2 `GET /local/nodegroups`
Returns every node group (= one chunk template per provider × chunk type) the Consumer Agent has cached from its latest Broker sync.
```
GET /local/nodegroups

Response 200:
{
  "nodeGroups": [
    {
      "id":                    "ng-provider-1-standard",
      "providerClusterId":     "provider-1",
      "providerLiqoClusterId": "liqo-provider-1-xxxx",
      "type":                  "standard",
      "minSize":               0,
      "maxSize":               3,
      "currentReserved":       1,
      "chunkResources":        {"cpu": "2", "memory": "4Gi", "nvidia.com/gpu": "0"},
      "cost":                  1.0,
      "topology":              {"zone": "eu-west-1a", "region": "eu-west-1"},
      "labels":                {"liqo.io/remote-cluster-id": "liqo-provider-1-xxxx"},
      "taints":                [{"key": "virtual-node.liqo.io/not-allowed", "effect": "NoExecute"}]
    }
  ],
  "generation": 42,
  "servedAt":   "2026-04-08T10:30:00Z",
  "cacheAgeSeconds": 3
}
```

#### 7.2.3 `POST /local/reservations`
Agent replies `202 Accepted` immediately with a `reservationId`; the gRPC server then polls `GET /local/reservations/{id}` every 500 ms until `Peered` or CA's internal deadline. This keeps CA's default gRPC timeout (10 s) uncoupled from the Liqo peering latency.
```
POST /local/reservations
Headers: X-Reservation-Id: <client-generated uuid>
Body:
{
  "providerClusterId": "provider-1",
  "nodeGroupId":       "ng-provider-1-standard",
  "chunkCount":        2,
  "chunkType":         "standard",
  "namespaces":        ["default"]
}

Response 202:
{
  "reservationId": "res-abc123",
  "status":        "Pending",
  "expiresAt":     "2026-04-08T10:35:00Z"
}
```

#### 7.2.4 `GET /local/reservations/{id}`
```
GET /local/reservations/res-abc123

Response 200:
{
  "reservationId":    "res-abc123",
  "status":           "Peered",
  "virtualNodeNames": ["liqo-node-provider-1-0", "liqo-node-provider-1-1"],
  "chunkCount":       2,
  "createdAt":        "2026-04-08T10:30:00Z",
  "activatedAt":      "2026-04-08T10:31:12Z",
  "message":          ""
}
```

#### 7.2.5 `DELETE /local/reservations/{id}`
```
DELETE /local/reservations/res-abc123?chunks=1

Response 202:
{
  "reservationId":       "res-abc123",
  "status":              "Unpeering",
  "remainingChunkCount": 1
}
```
Omit `?chunks=` to release the whole reservation.

#### 7.2.6 `GET /local/reservations`
```
GET /local/reservations

Response 200:
{ "reservations": [ /* 7.2.4 objects */ ], "total": 3 }
```

---

### 7.3 Consumer / Provider Agent → Broker  (REST over mTLS, **agent-initiated only**)

Base URL: `https://broker.central.example.com:8443/api/v1`. Mutual TLS required; `clusterId` derived from certificate CN. Each endpoint below is annotated with the agent role(s) that call it — the Broker never initiates calls in the opposite direction.

> **Implemented in:** `internal/broker/api/` (server.go wires the mux; per-route handlers in `advertisement.go`, `heartbeat.go`, `nodegroups.go`, `reservation.go`, `instructions.go`); `internal/broker/api/runnable.go` wraps the server as a leader-elected `manager.Runnable`. Client side: `internal/agent/client/endpoints.go` (`PostAdvertisement`, `PostHeartbeat`, `GetNodeGroups`, `PostReservation`, `DeleteReservation`, `GetInstructions`, `PostInstructionResult`). v1 omits `GET /api/v1/advertisements/{clusterId}` (§7.3.2) and `GET /api/v1/reservations/{id}` (§7.3.8) — both are read-only conveniences not yet wired.

#### 7.3.1 `POST /api/v1/advertisements`   *(provider, every 30 s — doubles as heartbeat; response piggybacks instructions)*
```
POST /api/v1/advertisements
Body:
{
  "clusterId":            "provider-1",
  "liqoClusterId":        "liqo-provider-1-xxxx",
  "resources":            {"cpu": "8", "memory": "16Gi", "nvidia.com/gpu": "0"},
  "topology":             {"zone": "qc-a", "region": "QC", "latitude": 45.6085, "longitude": -73.5493},
  "unitPrices":           {"cpu": "0.03", "memory": "0.004", "nvidia.com/gpu": "1.50"},
  "carbonIntensity":      25.0,                            // optional (Eco input); from mock-eco for this region
  "capacityScalePercent": {"cpu": 100, "memory": 50},      // optional; % of allocatable to donate (default 100)
  "liqoLabels":           {"liqo.io/type": "virtual-node"},
  "liqoTaints":           [{"key": "virtual-node.liqo.io/not-allowed", "effect": "NoExecute"}]
}

Response 200:
{
  "accepted":      true,
  "chunkCount":    4,
  "chunkResources":{"cpu": "2", "memory": "4Gi"},
  "nextReportIn":  "30s",
  "instructions":  [ /* zero or more ProviderInstruction objects (see 7.3.6) */ ]
}
```

#### 7.3.2 `GET /api/v1/advertisements/{clusterId}`   *(provider)*
Returns the Broker's current view of this provider's advertisement, including `Reserved` chunks. Used by the agent to preserve the Broker-managed `Reserved` field across advertisement re-submissions (same protocol as upstream `k8s-resource-brokering`).

#### 7.3.3 `POST /api/v1/heartbeat`   *(consumer, every 15 s)*
The body carries the consumer's current placement policy, read fresh from its `ConsumerPolicy` CRD each beat (§5.6), plus its **auto-discovered** location (its node IP geolocated via mock-geo) — the origin the **Latency** strategy measures distance from. All are optional: an omitted `placement` is the Broker default (no preference); omitted coordinates make Latency a no-op for this consumer.
```
POST /api/v1/heartbeat
Body: {
  "clusterId":     "consumer-1",
  "liqoClusterId": "liqo-consumer-1-xxxx",
  "placement":     {"type": "Latency"},     // optional; Price | Eco | Latency; omitted = Broker default
  "region":        "ENG",                   // optional; auto-discovered region code
  "city":          "London",                // optional; auto-discovered city (informational)
  "latitude":      51.5074,                 // optional; from mock-geo — the Latency origin
  "longitude":     -0.1278
}

Response 200: {"ackAt": "2026-04-08T10:30:00Z"}
```

#### 7.3.4 `GET /api/v1/nodegroups`   *(consumer)*
Returns the node groups visible to **this** consumer. Same schema as § 7.2.2, but the view is per-consumer: if the caller's last-heartbeated `ConsumerPolicy` sets a strategy, the Broker masks the list for that metric — to the single cheapest (`Price`) or greenest (`Eco`) provider with capacity per chunk type, giving the others `maxSize = currentReserved` (no head-room). For `Latency` it instead leaves the **top-3 nearest** growable and sets `latencyShortlist: true` + each candidate's `probeEndpoint`; the Consumer Agent's loopback API UDP-probes them and re-masks to the lowest measured RTT before CA sees the list. With no policy, no provider carrying the metric, or (for `Latency`) no consumer coordinates, the full list is returned unchanged (§4.1, "Metric-based placement").

#### 7.3.5 `POST /api/v1/reservations`   *(consumer — synchronous decision)*
Broker runs the decision engine inline and returns the reservation record immediately. At this point `phase` is `Pending` or `GeneratingKubeconfig`; the peering step is handled asynchronously via polling (§ 7.3.6).
```
POST /api/v1/reservations
Headers: X-Reservation-Id: <uuid>
Body:
{
  "providerClusterId": "provider-1",
  "nodeGroupId":       "ng-provider-1-standard",
  "chunkCount":        2,
  "chunkType":         "standard",
  "namespaces":        ["default"]
}

Response 201:
{
  "reservationId":       "res-abc123",
  "status":              "GeneratingKubeconfig",
  "providerClusterId":   "provider-1",
  "providerLiqoClusterId":"liqo-provider-1-xxxx",
  "chunkCount":          2,
  "chunkType":           "standard",
  "resources":           {"cpu": "2", "memory": "4Gi"},
  "createdAt":           "2026-04-08T10:30:00Z",
  "expiresAt":           "2026-04-08T10:35:00Z"
}
```

#### 7.3.6 `GET /api/v1/instructions`   *(both agents, polled every 5 s)*
Returns the list of pending instructions for this cluster (derived from the CRDs owned by the Broker). Subject to per-cluster rate limiting (10 burst / 5 sustained rps).

Body for a **provider** call returns `ProviderInstruction` objects:
```
Response 200:
{
  "instructions": [
    {
      "id":                    "inst-res-abc123-genkube",
      "kind":                  "GenerateKubeconfig",
      "reservationId":         "res-abc123",
      "consumerClusterId":     "consumer-1",
      "consumerLiqoClusterId": "liqo-consumer-1-xxxx",
      "chunkCount":            2,
      "lastChunk":             false,
      "issuedAt":              "2026-04-08T10:30:02Z",
      "expiresAt":             "2026-04-08T10:35:00Z"
    }
  ]
}
```

Body for a **consumer** call returns `ReservationInstruction` objects, with the kubeconfig inlined for `Peer`:
```
Response 200:
{
  "instructions": [
    {
      "id":                     "inst-res-abc123-peer",
      "kind":                   "Peer",
      "reservationId":          "res-abc123",
      "providerClusterId":      "provider-1",
      "providerLiqoClusterId":  "liqo-provider-1-xxxx",
      "chunkCount":             2,
      "lastChunk":              false,
      "kubeconfig":             "<base64 PEM kubeconfig, inlined>",
      "resourceSliceResources": {"cpu": "2", "memory": "4Gi", "nvidia.com/gpu": "0"},
      "namespaces":             ["default"],
      "issuedAt":               "2026-04-08T10:30:14Z",
      "expiresAt":              "2026-04-08T10:35:00Z"
    }
  ]
}
```

Redelivery semantics: the Broker keeps returning an instruction while `status.enforced == false`. Only a matching `POST /instructions/{id}/result` clears it from subsequent polls. This is exactly the behaviour of upstream `k8s-resource-brokering`'s `GetInstructions` handler.

#### 7.3.7 `POST /api/v1/instructions/{id}/result`   *(both agents)*
Report the outcome of an instruction. Keeps large payloads (kubeconfig bytes, error messages) out of CRD `status` and out of etcd watch streams.

Provider, after running `liqoctl generate peering-user`:
```
POST /api/v1/instructions/inst-res-abc123-genkube/result
Headers: X-Reservation-Id: res-abc123
Body:
{
  "status":  "Succeeded",
  "payload": {
    "kind":       "KubeconfigPayload",
    "kubeconfig": "<base64 PEM>",
    "expiresAt":  "2026-04-08T11:30:00Z"
  }
}

Response 200: {"accepted": true}
```

Consumer, after `liqoctl peer` + ResourceSlice creation:
```
POST /api/v1/instructions/inst-res-abc123-peer/result
Headers: X-Reservation-Id: res-abc123
Body:
{
  "status":  "Succeeded",
  "payload": {
    "kind":               "PeerPayload",
    "virtualNodeNames":   ["liqo-node-provider-1-0", "liqo-node-provider-1-1"],
    "resourceSliceNames": ["rs-res-abc123-0", "rs-res-abc123-1"]
  }
}
```

Unpeer result:
```
POST /api/v1/instructions/inst-res-abc123-unpeer/result
Body:
{
  "status":  "Succeeded",
  "payload": {
    "kind":           "UnpeerPayload",
    "releasedChunks": 1,
    "tunnelDropped":  false
  }
}
```

Reconcile result (agent's authoritative view):
```
POST /api/v1/instructions/inst-reconcile-xyz/result
Body:
{
  "status":  "Succeeded",
  "payload": {
    "kind":              "ReconcilePayload",
    "advertisement":     { /* § 7.3.1 body, agent's current truth (provider only) */ },
    "virtualNodeStates": [ /* condensed view of local state */ ]
  }
}
```

Failure path:
```
Body:
{
  "status":  "Failed",
  "error":   {"code": "UpstreamError", "message": "liqoctl peer exited with status 1"}
}
```

#### 7.3.8 `GET /api/v1/reservations/{id}`   *(both agents)*
Same schema as § 7.2.4. Used by the consumer agent to populate its local cache when a gRPC-server call misses.

#### 7.3.9 `DELETE /api/v1/reservations/{id}`   *(consumer — full or partial release)*
```
DELETE /api/v1/reservations/res-abc123?chunks=1

Response 202:
{
  "reservationId":       "res-abc123",
  "status":              "Unpeering",
  "remainingChunkCount": 1
}
```

---

### 7.4 Summary of Endpoints

| Surface | Method & Path | Direction | Purpose |
|---|---|---|---|
| 7.1 | gRPC `externalgrpc.proto` (15 methods) | CA ↔ gRPC Server | CA cloud-provider contract |
| 7.2.1 | `GET  /local/health` | gRPC → Consumer Agent | liveness |
| 7.2.2 | `GET  /local/nodegroups` | gRPC → Consumer Agent | list node groups |
| 7.2.3 | `POST /local/reservations` | gRPC → Consumer Agent | reserve chunks (202) |
| 7.2.4 | `GET  /local/reservations/{id}` | gRPC → Consumer Agent | reservation status |
| 7.2.5 | `DELETE /local/reservations/{id}` | gRPC → Consumer Agent | release (full / partial) |
| 7.2.6 | `GET  /local/reservations` | gRPC → Consumer Agent | list reservations |
| 7.3.1 | `POST /api/v1/advertisements` | Provider Agent → Broker | advertise + heartbeat + piggyback |
| 7.3.2 | `GET  /api/v1/advertisements/{clusterId}` | Provider Agent → Broker | preserve `Reserved` field |
| 7.3.3 | `POST /api/v1/heartbeat` | Consumer Agent → Broker | 15 s liveness |
| 7.3.4 | `GET  /api/v1/nodegroups` | Consumer Agent → Broker | list node groups |
| 7.3.5 | `POST /api/v1/reservations` | Consumer Agent → Broker | synchronous decision |
| 7.3.6 | `GET  /api/v1/instructions` | Both Agents → Broker | poll pending instructions every 5 s |
| 7.3.7 | `POST /api/v1/instructions/{id}/result` | Both Agents → Broker | report outcome + payload |
| 7.3.8 | `GET  /api/v1/reservations/{id}` | Both Agents → Broker | reservation lookup |
| 7.3.9 | `DELETE /api/v1/reservations/{id}` | Consumer Agent → Broker | release |

**Non-mTLS HTTP surfaces.** The table above is the mTLS agent↔broker API. Separately, a few **plain-HTTP** surfaces serve operators and the placement strategies (none on the agent↔broker critical path):

| Surface | Method & Path | Direction | Purpose |
|---|---|---|---|
| §4.8a | `GET /` · `GET /api/v1/overview` | browser → Broker (NodePort 30444) | read-only dashboard |
| §4.8c | `GET /` · `GET /api/state` · `POST /api/{policy,workload,reservation}` (consumer) / `POST /api/{prices,capacity,renewable}` (provider) | browser → Agent console (NodePort 30445) | set per-cluster knobs (location is auto-discovered, shown read-only) |
| §4.7 | `GET /carbon?region=` + `GET /carbon/forecast?region=` (mock-eco) · `GET /json/<ip>` (mock-geo) | Agent → Mock services | carbon + auto-discovered location |

---

## 8. Execution Flows

### 8.1 Registration Flow

> **Source-of-truth diagram:** [`diagrams/registration.mmd`](diagrams/registration.mmd).

```
PROVIDER CLUSTER                      CENTRAL CLUSTER
─────────────────                     ───────────────
Provider Agent                        Broker
     │                                   │
     │ POST /api/v1/advertisements       │
     │ { resources, liqoClusterId,       │
     │   topology, unitPrices }          │
     │──── mTLS ───────────────────────>│
     │                                   │── Calculate chunks
     │                                   │── Upsert ClusterAdvertisement
     │ 200 + {chunkCount, instructions: []} ── Update lastSeen
     │<──────────────────────────────────│
     │                                   │
     │ (repeats every 30 s)              │
     │ GET /api/v1/instructions (5 s)    │── returns [] until something is pending
     │<──────────────────────────────────│


CONSUMER CLUSTER                      CENTRAL CLUSTER
─────────────────                     ───────────────
Consumer Agent                        Broker
     │                                   │
     │ POST /api/v1/heartbeat (15 s)     │── Update lastSeen, create ConsumerRecord on first call
     │ { liqoClusterId, placement }      │── store placement policy (from ConsumerPolicy CRD)
     │──── mTLS ───────────────────────>│
     │                                   │
     │ GET /api/v1/instructions (5 s)    │── returns [] until something is pending
     │<──────────────────────────────────│
```

The Broker never needs to dial either cluster.

> **Implemented in:** Provider Agent — `internal/agent/provider/advertise/publisher.go` (30 s POST loop) + `internal/agent/provider/snapshot/snapshot.go` (allocatable + topology); Consumer Agent — `internal/agent/consumer/heartbeat/heartbeat.go` (15 s POST loop; also reads the `ConsumerPolicy` CRD each beat and sends it as `placement`); Broker side — `internal/broker/api/advertisement.go`, `internal/broker/api/heartbeat.go`, `internal/broker/api/registry.go` (in-memory `ConsumerRegistry`, now also storing each consumer's placement policy).

### 8.2 Scale-Up Flow (Complete)

![Scale Up](diagrams/scale-up-execution-flow.png)

> **Source-of-truth diagram:** [`diagrams/scale-up.mmd`](diagrams/scale-up.mmd) — includes the `liqoctl generate peering-user` (provider) and `liqoctl peer` (consumer) exec hops the v3.0 PNG predates.

```
Step 1: CA detects unschedulable pods.
Step 2: CA calls Refresh() → gRPC server → Agent's /local/nodegroups (served from cache).
        ↳ The Broker built this list FOR THIS CONSUMER. If the consumer's
          ConsumerPolicy is type:Price, the Broker already masked it to the
          cheapest priced provider with capacity (others have max==reserved,
          i.e. no head-room) — so provider selection has happened HERE, before
          CA decides anything.
Step 3: CA calls NodeGroups() → e.g. [ng-provider-1-standard(min=0,max=3), ng-provider-2-gpu(min=0,max=2)].
Step 4: CA calls NodeGroupTemplateNodeInfo() → chunk-sized fake Node per group.
Step 5: CA simulates: "can my pending pods fit on a node from group X?"
Step 6: CA estimator: "how many nodes from group X do I need?"
Step 7: CA's expander (neutral, e.g. least-waste — NOT price) picks among the
        groups with head-room. Under price masking only the cheapest provider
        has any, so the Broker — not CA — has effectively chosen it. CA never
        sees price.
Step 8: CA calls NodeGroupIncreaseSize(id="ng-provider-1-standard", delta=2).

→ gRPC Server:
  Step  9: POST http://agent:8080/local/reservations { provider-1, 2 chunks }.
  Step 10: Creates VirtualNodeState CRDs: chunk-0 (Creating), chunk-1 (Creating).

→ Consumer Agent:
  Step 11: POST https://broker.../api/v1/reservations (mTLS) — SYNCHRONOUS.

→ Broker:
  Step 12: Validates availability (3 ≥ 2) ✓.
  Step 13: Creates Reservation (phase=GeneratingKubeconfig, expiresAt=+15m).
  Step 14: Updates ClusterAdvertisement (reservedChunks += 2).
  Step 15: Creates a ProviderInstruction{Kind:GenerateKubeconfig} CRD targeted at provider-1.
  Step 16: Returns 201 { reservationId, status: "GeneratingKubeconfig", provider… } synchronously.

→ Consumer Agent:
  Step 17: Returns 202 { reservationId, status:"GeneratingKubeconfig" } to gRPC server (unblocks CA chain).

→ gRPC Server:
  Step 18: Starts polling GET /local/reservations/{id} every 500 ms (bounded by CA's MaxNodeProvisionDuration).

→ Provider Agent (next 5 s poll OR piggybacked in its next /advertisements response):
  Step 19: Receives ProviderInstruction{GenerateKubeconfig, consumer-1, consumerLiqoClusterId…}.
  Step 20: Runs `liqoctl generate peering-user --consumer-cluster-id liqo-consumer-1-xxxx`.
  Step 21: POST /api/v1/instructions/{id}/result {status:"Succeeded", payload:{kubeconfig}}.

→ Broker:
  Step 22: Marks the ProviderInstruction as Enforced:true (removed from future polls).
  Step 23: Stores kubeconfig bytes inside a new ReservationInstruction{Kind:Peer} targeted at consumer-1.
  Step 24: Reservation phase → KubeconfigReady → Peering.

→ Consumer Agent (next 5 s poll):
  Step 25: GET /api/v1/instructions returns ReservationInstruction{Peer, kubeconfig inlined, chunkCount=1…}.
  Step 26: Writes kubeconfig to a Secret on the consumer cluster.
  Step 27: Runs `liqoctl peer … --create-resource-slice=false` (networking + auth + gateway only).
  Step 28: Creates ResourceSlice rs-<reservationID> (cpu=2, memory=4Gi) in liqo-tenant-<provider>.
  Step 29: NamespaceOffloading is operator-stamped, not created here.
  Step 30: Liqo materializes ONE VirtualNode, named after the slice (rs-<reservationID>).
  Step 31: POST /api/v1/instructions/{id}/result {status:"Succeeded", payload:{resourceSliceNames}}.

  A 2-chunk scale-up is TWO Reservations, each running the above independently and
  yielding its own node. One Reservation = one chunk = one ResourceSlice = one node,
  because Liqo derives the node name from the slice name — so a single Reservation can
  never produce more than one node, whatever its chunkCount said.

→ Broker:
  Step 32: Marks ReservationInstruction Enforced:true; Reservation phase → Peered; stores virtualNodeNames.

→ gRPC Server:
  Step 33: Its 500 ms poll now sees phase=Peered + virtualNodeNames → updates VirtualNodeState → Running.
  Step 34: Returns success to CA's NodeGroupIncreaseSize.

→ CA:
  Step 35: NodeGroupNodes() returns instances with InstanceRunning status.
  Step 36: Scheduler places pending pods on the new virtual nodes.
```

**Typical latency:** 15-30 s from step 1 to step 36. The two polling gaps (steps 19, 25) add ≤ 5 s each in the worst case and ≤ 0 s when advertisement-piggyback delivers the provider's instruction.

> **Implemented in:** `internal/grpcserver/rpc_mutating.go` (`NodeGroupIncreaseSize`); `internal/broker/api/reservation.go` (`POST /api/v1/reservations` synchronous decision); `internal/controller/broker/reservation_controller.go` (`Pending → GeneratingKubeconfig → KubeconfigReady → Peering → Peered` phase machine + provider/reservation-instruction emission); `internal/agent/provider/instructions/generatekubeconfig.go` (provider side, `liqoctl generate peering-user` shell-out); `internal/agent/consumer/instructions/peer.go` (consumer side, kubeconfig secret + `liqoctl peer` + ResourceSlice + NamespaceOffloading + VirtualNodeState create); `internal/controller/autoscaling/virtualnodestate_controller.go` (projects Liqo `VirtualNode` Ready onto VNS). End-to-end coverage: `internal/integration/scaleup_test.go` and `test/e2e/` (multi-Kind).

### 8.3 Scale-Down Flow (Complete)

![Scale Down](diagrams/scale-down-execution-flow.png)

> **Source-of-truth diagram:** [`diagrams/scale-down.mmd`](diagrams/scale-down.mmd) — includes the `liqoctl unpeer` exec hop the v3.0 PNG predates.

```
Step 1: CA detects an underutilized virtual node for > 5 minutes.
Step 2: CA drains pods.
Step 3: CA calls NodeGroupDeleteNodes(id="ng-provider-1-standard", nodes=[node-xyz]).

→ gRPC Server:
  Step 4: Looks up VirtualNodeState for node-xyz → reservation res-abc123, chunk-0.
  Step 5: DELETE http://agent:8080/local/reservations/res-abc123?chunks=1.
  Step 6: Marks VirtualNodeState chunk-0 → Deleting.

→ Consumer Agent:
  Step 7: DELETE https://broker.../api/v1/reservations/res-abc123?chunks=1 (mTLS).

→ Broker:
  Step 8:  Reservation phase → Unpeering; Reservation.Spec.chunkCount -= 1.
  Step 9:  Creates a ReservationInstruction{Kind:Unpeer, resourceSliceNames:[rs-chunk-0], lastChunk:false} for consumer-1.
  Step 10: If this was the last chunk of this reservation → also creates ProviderInstruction{Kind:Cleanup, lastChunk:true} for provider-1.
  Step 11: Returns 202 to the consumer agent → consumer agent returns 202 to gRPC server.

→ Consumer Agent (next 5 s poll):
  Step 12: Receives ReservationInstruction{Unpeer, resourceSliceNames:[rs-chunk-0]}.
  Step 13: Deletes the ResourceSlice.
  Step 14: If lastChunk == true → runs `liqoctl unpeer`.
  Step 15: POST /instructions/{id}/result {status:"Succeeded", payload:{releasedChunks:1, tunnelDropped:<bool>}}.

→ Provider Agent (only if lastChunk == true, next 5 s poll):
  Step 16: Receives ProviderInstruction{Cleanup}; tears down the peering-user kubeconfig.
  Step 17: POST /instructions/{id}/result {status:"Succeeded"}.

→ Broker:
  Step 18: Reservation phase → Released; availableChunks += 1 (chunk returns to the pool for any consumer).

→ gRPC Server (next Refresh):
  Step 19: GET /local/reservations/res-abc123 → 404 or Released.
  Step 20: Deletes VirtualNodeState chunk-0; next NodeGroups() reflects the freed chunk in maxSize.
```

> **Implemented in:** `internal/grpcserver/rpc_mutating.go` (`NodeGroupDeleteNodes`); `internal/broker/api/reservation.go` (`DELETE /api/v1/reservations/{id}` — v1 supports full release only; the `LastChunk: true` hardcode lives in `reservation_controller.go`'s `handleUnpeering`); `internal/controller/broker/reservation_controller.go` (`handleUnpeering` + `handleTerminal`, emitting a `ProviderInstruction{Cleanup}` **and** an un-owned consumer `ReservationInstruction{Cleanup}`, then GC'ing the terminal Reservation after `DefaultTerminalReservationTTL`); `internal/agent/consumer/instructions/unpeer.go` (delete VNS → delete ResourceSlice → `liqoctl unpeer` → delete the leftover `ForeignCluster` shell on `LastChunk`) and `internal/agent/provider/instructions/cleanup.go`.

> **Chunk-release invariant.** `ClusterAdvertisement.Status.ReservedChunks` is decremented by *exactly one* path per Reservation, gated by the `federation-autoscaler.io/chunks-released` annotation on the Reservation (see `api/broker/v1alpha1/common_types.go`, `IsChunksReleased` / `MarkChunksReleased` helpers). The API `DELETE /api/v1/reservations/{id}` handler is the normal release path (stamps the annotation after decrementing); the reservation reconciler's `handleTerminal` is the safety net that covers any non-Unpeering terminal phase (Failed / Expired / etc.) and only decrements when the annotation is absent. Without this idempotency marker the counter drifts — provider `AvailableChunks` gets stuck at 0 forever after any Reservation that died mid-flight without going through the DELETE path. Do not add a third release path.

### 8.4 Reservation Timeout Flow

```
Broker creates Reservation at T=0 with expiresAt = T+15m (default reservation-timeout).

Broker reconciliation loop (every 30 s):
  If now > reservation.expiresAt AND phase ∈ {Pending, GeneratingKubeconfig, KubeconfigReady, Peering}:
    → Phase → Expired.
    → Release chunks (reservedChunks -= N).
    → Delete any still-pending ProviderInstruction/ReservationInstruction for this reservation.

Agents learn about expiry passively:
  - Consumer Agent's next GET /instructions no longer sees the Peer instruction.
  - gRPC Server's next GET /local/reservations/{id} returns status=Expired → VirtualNodeState → Failed.
  - CA may retry on a different provider.
```

> **Implemented in:** `internal/controller/broker/reservation_controller.go` — expiry guard inside the phase reconciler flips non-terminal Reservations past `Spec.ExpiresAt` to `Expired` and emits cleanup instructions. `instruction.go`'s `DefaultEnforcedTTL` (5 min) garbage-collects the instructions after delivery.

### 8.5 Provider Heartbeat Timeout Flow

```
Provider-1 last advertisement at T=0.
Broker agent-heartbeat-timeout = 30 s (one missed beat).

Broker reconciliation loop (every 30 s):
  If now - lastSeen > agent-heartbeat-timeout:
    → Mark ClusterAdvertisement.available = false.
    → Exclude provider-1 from /nodegroups and from new /reservations decisions.
    → Existing Active reservations are preserved (peering may still be healthy);
      if the peering actually dropped, Liqo itself will surface that locally and the consumer
      agent's reconcile loop will release the ResourceSlices.

When provider-1 agent returns:
  → Next advertisement updates lastSeen.
  → available = true.
  → Provider-1 re-appears in nodegroup responses.
```

> **Implemented in:** `internal/controller/broker/clusteradvertisement_controller.go` (`StaleAfter` freshness flips `Status.Available`); `internal/controller/broker/reservation_controller.go` (`checkProviderAvailable` + `requestsForAdvertisement` watch fail-fasts non-terminal Reservations whose provider has dropped).

### 8.6 Reconciliation / Drift Recovery

```
Triggers:
  - Broker pod restart or leader change.
  - Broker detects a mismatch between its CRD state and an agent-reported action
    (e.g., VirtualNode exists on consumer but no matching Reservation on Broker).

Steps:
  1. Broker creates a ProviderInstruction{Kind:Reconcile} or ReservationInstruction{Kind:Reconcile}
     for every affected cluster.
  2. On the next 5 s poll, the agent receives the reconcile instruction.
  3. Agent assembles its authoritative view (advertisement content, list of ResourceSlices /
     VirtualNodeStates, active peerings).
  4. Agent POSTs /instructions/{id}/result { payload: ReconcilePayload }.
  5. Broker diff-applies the snapshot to its CRDs (create missing Reservations, release orphans).
```

> **Implemented in:** `internal/agent/consumer/instructions/reconcile.go` + `internal/agent/provider/instructions/reconcile.go` (per-role snapshot handlers). The broker's reconcile-instruction emission on startup is part of `internal/controller/broker/`; the diff-apply pass is a v2 deliverable — the v1 reconcile path just reports current state.

---

## 9. Failure Handling and Reconciliation

### 9.1 gRPC Server Reconciliation Loop (every 30 s)

| Check | Action |
|---|---|
| VirtualNodeState is `Running` but no matching Liqo virtual node exists | Mark as `Failed`, issue `DELETE /local/reservations/{id}` |
| VirtualNodeState is `Creating` for longer than `reservation-timeout` | Mark as `Failed` (Broker has already expired the reservation) |
| Virtual node exists but no matching VirtualNodeState | Log warning (manual creation suspected) |
| Agent unreachable | Serve cached node-group data, retry on next loop |

> **Implemented in:** `internal/controller/autoscaling/virtualnodestate_controller.go` (timer-based `DefaultVirtualNodeStateRequeue=30s` safety net plus the Liqo `VirtualNode` watch wired in `SetupWithManager`). The cached-node-group fallback is the gRPC server's stateless behaviour — every RPC re-fetches via the loopback REST.

### 9.2 Consumer Agent Reconciliation Loop (every 60 s)

| Check | Action |
|---|---|
| ResourceSlice exists but corresponding VirtualNodeState is `Deleting` / `Failed` | Delete the ResourceSlice |
| Liqo peering is broken for a provider with active ResourceSlices | Emit event; next Broker Reconcile instruction will surface it |
| NamespaceOffloading missing for namespaces with active virtual nodes | Recreate NamespaceOffloading CR |
| Local cache older than 2 × poll interval | Force a fresh `GET /api/v1/nodegroups` |

> **Implemented in:** `internal/agent/consumer/instructions/reconcile.go` (Reconcile-instruction handler) + the Liqo CR delete helpers in `liqo.go`. Drift detection for orphaned ResourceSlices / missing NamespaceOffloading is partial in v1 — the consumer agent only deletes Liqo CRs in response to explicit Unpeer/Cleanup instructions today, not via an independent 60 s loop.

### 9.3 Broker Reconciliation Loop (every 30 s)

| Check | Action |
|---|---|
| Reservation past `expiresAt` and not Peered | Mark `Expired`, release chunks, delete pending instructions |
| Provider agent not seen within `agent-heartbeat-timeout` | Mark `Unavailable`; hide from /nodegroups |
| Consumer agent not seen within `agent-heartbeat-timeout` | Flag; after 2 × timeout, create Cleanup ProviderInstructions on all providers peered with it |
| Orphan `ProviderInstruction` / `ReservationInstruction` (reservation gone) | Delete |
| Enforced instructions older than 24 h | GC |
| Terminal reservations older than 7 days | GC |

> **Implemented in:** `internal/controller/broker/reservation_controller.go` (expiry guard + `checkProviderAvailable` watch off `ClusterAdvertisement`), `internal/controller/broker/clusteradvertisement_controller.go` (`StaleAfter` + `Available` flip), `internal/controller/autoscaling/instruction.go` (`DefaultEnforcedTTL=5m` instruction GC). Terminal Reservations (`Released`/`Failed`/`Expired`) are garbage-collected by `reservation_controller.go`'s `handleTerminal` once `Status.TerminatedAt` is older than `DefaultTerminalReservationTTL` (15 min; the reconciler's `TerminalTTL` field overrides for tests).

### 9.4 Idempotency Contract

- Every reservation-scoped call carries `X-Reservation-Id` in header + body; the agent refuses requests where they mismatch.
- Agents cache `POST /instructions/{id}/result` replies for `idempotency-cache-ttl` so a retried instruction execution reports the same result without re-running `liqoctl`.
- The Broker's `GET /instructions` handler returns the same instruction repeatedly while `status.enforced == false`; result submission is the only thing that clears it. Agents MUST treat every execution as idempotent (no-op on re-invocation).

> **Implemented in:** Broker side — `internal/broker/api/reservation.go` derives a deterministic `Reservation` CR name from the `X-Reservation-Id` header so retries collapse onto the same CR; `internal/controller/autoscaling/instruction.go` flips `Status.Enforced` on first successful result; `internal/broker/api/middleware.go` provides the request-id middleware. Agent side — every `internal/agent/consumer/instructions/*.go` and `internal/agent/provider/instructions/*.go` handler is idempotent on AlreadyExists / NotFound.

### 9.5 Agent Crash Recovery

- `replicas: 1` with `strategy: Recreate`: Kubernetes replaces a crashed agent pod without two pods ever polling concurrently.
- The replacement pod polls `GET /instructions` on startup and re-receives any instruction that was in flight → re-executes idempotently.
- Agent-side caches are in-memory; a restart loses them, but Broker state is authoritative and the next poll re-populates.

> **Implemented in:** `config/agent/base/deployment.yaml` (`replicas: 1` + `strategy: Recreate`); `internal/agent/poller/poller.go` (single-goroutine poll loop; refuses to start two instances).

### 9.6 Broker Leader Failover

- HTTP server is stateless; Lease-based leader election swaps the serving pod.
- Agents experience at most one request failure during swap, absorbed by their 3-attempt retry with backoff.
- On becoming leader, a pod issues `Reconcile` instructions to every known cluster to resynchronize state (§ 8.6).

> **Implemented in:** `internal/manager/manager.go` (`LeaderElectionID = "broker.federation-autoscaler.io"`); `internal/broker/api/runnable.go` (`NeedLeaderElection() = true` so only the leader serves API traffic); `config/broker/deployment.yaml` runs `replicas: 2`. The reconcile-on-leader-change emission is part of `internal/controller/broker/`'s controller lifecycle.

---

## 10. Security

### 10.1 Communication Security

| Channel | Protocol | Auth |
|---|---|---|
| CA ↔ gRPC Server | gRPC, in-cluster | localhost / in-cluster mTLS |
| gRPC Server ↔ Consumer Agent | HTTP, in-cluster | ClusterIP Service; optional ServiceAccount token |
| Consumer Agent → Broker | HTTPS, cross-cluster, **agent-initiated only** | mTLS (Consumer Agent = client) |
| Provider Agent → Broker | HTTPS, cross-cluster, **agent-initiated only** | mTLS (Provider Agent = client) |

No inbound port is required on any agent cluster. Consumer and provider clusters need only outbound egress to the Broker URL.

> **Implemented in:** `internal/broker/api/tls.go` (broker mTLS server config + CN extraction), `internal/agent/client/tls.go` (agent mTLS client), `internal/grpcserver/tls.go` (externalgrpc mTLS server). All three load `tls.crt` / `tls.key` / `ca.crt` from a cert-manager-produced Secret mounted at `/etc/federation-autoscaler/tls`.

### 10.2 Certificate Management

- **Broker:** server certificate for its `/api/v1/*` endpoints.
- **Consumer Agent and Provider Agent:** each carries its own client certificate with `CN = <Liqo cluster ID>` used when calling the Broker. No server certificate is required (provider agents have no listener; consumer agents expose only a ClusterIP Service in-cluster).
- Certificates are issued by a federation CA (e.g. cert-manager `ClusterIssuer`) and delivered to the agent as a Kubernetes Secret at onboarding time. Recommended rotation: 90 days.

> **Implemented in:** `config/broker/certmanager.yaml` (selfsigned root → `federation-autoscaler-ca` (`IsCA=true`, 10y) → `federation-autoscaler-ca-issuer` → broker server cert, 1y / 30d rotation). `config/agent/base/certificate.yaml` and `config/grpc-server/certificate.yaml` chain to the same CA-Issuer. The e2e suite stamps CN=`<clusterId>` via `test/e2e/bootstrap/agent_config.go`.

### 10.3 Authentication & Authorisation

- The Broker's mTLS middleware extracts `clusterId` from the client certificate CN. Neither the Consumer Agent nor the Provider Agent can spoof another cluster's identity.
- Every endpoint verifies that `clusterId` in the payload (if present) matches the CN; mismatch → `403 Forbidden`.
- A Provider Agent cannot reserve resources (it only publishes advertisements); a Consumer Agent cannot publish advertisements. The handler selects the allowed set of operations from the cert's role attribute (or a namespace convention that encodes the role).

> **Implemented in:** `internal/broker/api/middleware.go` (`ClusterIDMiddleware` + 403-on-mismatch); `internal/broker/api/tls.go` (`extractClientCN`). Per-endpoint role gating (provider-only / consumer-only) is not yet implemented — any valid mTLS cert can call any handler today, gated only by CN-vs-payload cross-check.

### 10.4 Kubeconfig Security

- The peering-user kubeconfig is **limited scope** — produced by `liqoctl generate peering-user`, it grants only the permissions Liqo needs.
- Flow: Provider Agent → Broker (via `/instructions/{id}/result`, mTLS) → stored in the Broker's pending `ReservationInstruction` → Consumer Agent (inlined in the `/instructions` polling response, mTLS) → stored as a Kubernetes Secret on the consumer cluster.
- The kubeconfig is *not* written to the Broker's etcd CRD `status` (which would show up in watch streams). It is stored on disk within the Broker pod's PVC-backed instruction blob, or held in memory and replayed from the provider's idempotency cache on agent restart.
- Agent-side idempotency caches hold the kubeconfig for `idempotency-cache-ttl` (10 min) to tolerate retries; it is cleared thereafter.

> **Implemented in:** `internal/broker/api/instructions.go` (loads kubeconfig from a Broker-cluster Secret on each `/api/v1/instructions` poll and inlines it into the response; never written back to CRD status); `internal/agent/consumer/instructions/secrets.go` (`persistKubeconfig` writes the inlined bytes to a Secret on the consumer cluster); `internal/agent/consumer/instructions/peer.go` stages the kubeconfig under `/tmp` for `liqoctl peer` and removes it on exit.

### 10.5 Replay Protection

- Every reservation-scoped call carries `X-Reservation-Id`; the Broker rejects calls missing it.
- Both agents refuse to execute a second instruction for a reservation that is already in a terminal state.
- Both agents compare `reservationId` in the body against the header and the authenticated `clusterId`.

> **Implemented in:** `internal/broker/api/reservation.go` (rejects `POST /api/v1/reservations` with missing `X-Reservation-Id`; rejects body / header / CN mismatches with 403). Terminal-phase rejection on the agent side is implicit — Peer/Unpeer handlers are idempotent and the broker stops re-emitting the instruction after enforcement.

### 10.6 Rate Limiting

- Per-cluster token bucket on `GET /api/v1/instructions`: 10 rps burst, 5 rps sustained. Violations → `429 TooManyRequests`; the calling agent's backoff handles it.
- Per-cluster bucket on `POST /api/v1/reservations` (Consumer Agent only): 2 rps (prevents reservation-spam DoS).

> **Implemented in:** `internal/broker/api/middleware.go` (`rateLimitMiddleware` token bucket — 10 burst / 5 sustained on `/api/v1/instructions`, keyed by clusterID from the cert CN). The dedicated 2 rps bucket on `/api/v1/reservations` is not yet wired separately — both endpoints share the per-cluster bucket today.

---

## 11. Integration Plan with Existing Projects

### 11.1 From k8s-resource-brokering (Broker + Consumer/Provider Agents)

**Reuse as-is:**
- REST API server with mTLS (server.go + middleware)
- Existing handlers: `PostAdvertisement`, `GetAdvertisement`, `PostReservation`, `GetInstructions`
- Agent HTTP client: `HTTPCommunicator` with exponential backoff (`doWithRetry`) — shared by both agent roles
- `ClusterAdvertisement`, `Reservation`, `Advertisement`, `ProviderInstruction`, `ReservationInstruction` CRDs
- Decision engine interface and resource-locking utility
- mTLS cert loading + `ValidateClientCertificate` middleware

**Modify (additive only, `v1alpha1` stays):**

*Broker:*
- `ClusterAdvertisement` — add `liqoClusterId`, `topology`, `clusterType`, `unitPrices` (per-resource price-per-unit-per-hour), chunk status (`totalChunks`, `reservedChunks`, `availableChunks`)
- New `ConsumerPolicy` CRD (consumer cluster, `autoscaling.federation-autoscaler.io`) — the consumer's placement policy (`placement.type: Price`), pushed to the Broker on the heartbeat (§5.6)
- `Reservation` — add `consumerLiqoClusterId`, `providerLiqoClusterId`, `chunkCount`, `chunkType`, `virtualNodeNames`; extend phase enum with `GeneratingKubeconfig | KubeconfigReady | Peering | Peered | Unpeering | Released`
- `ProviderInstruction` — add `kind` (`GenerateKubeconfig | Cleanup | Reconcile`), `consumerLiqoClusterId`, `chunkCount`, `lastChunk`
- `ReservationInstruction` — add `kind` (`Peer | Unpeer | Cleanup | Reconcile`), `providerLiqoClusterId`, `resourceSliceNames`, `chunkCount`, `lastChunk`, in-response kubeconfig field (not stored in status)
- Add chunk-calculation logic driven by the `chunk-config` ConfigMap
- Extend `PostAdvertisement` response to piggyback pending `ProviderInstruction` records
- Add `POST /api/v1/instructions/{id}/result` endpoint
- Add `POST /api/v1/heartbeat` endpoint (Consumer Agent only)
- Add rate-limiting middleware (10/5 rps per cluster on `/instructions`, 2 rps on `/reservations`)
- Add per-consumer, price-based provider selection: compute a per-chunk cost from each provider's `unitPrices`, and mask `GET /api/v1/nodegroups` to the cheapest priced provider with capacity for consumers whose pushed `ConsumerPolicy` is `type: Price`

*Consumer Agent (new binary, shared-code base):*
- Add `/local/*` HTTP listener (ClusterIP) consumed by the gRPC server
- Add instruction executors: `Peer`, `Unpeer`, `Cleanup`, `Reconcile`
- Add `ResourceSlice` / `NamespaceOffloading` management and `liqoctl peer` / `liqoctl unpeer` execution capability
- Add a 15 s heartbeat loop and a synchronous reservation-forwarder (`POST` / `DELETE /api/v1/reservations`)
- Add local in-memory cache merging Broker polls + synchronous responses

*Provider Agent (new binary, shared-code base):*
- Add instruction executors: `GenerateKubeconfig`, `Cleanup`, `Reconcile`
- Add `liqoctl generate peering-user` execution capability
- Add a 30 s advertisement loop (`POST /api/v1/advertisements`) that also carries piggybacked instructions on the response
- No local HTTP listener

*Both agents (shared code):*
- Add idempotency cache keyed by `reservationId + instruction kind`
- Add 5 s `GET /api/v1/instructions` polling loop with exponential-backoff retrier
- Role selected at startup via CLI flag (`--role=consumer|provider`) — one binary, two Deployments

**Remove:**
- Any Broker-initiated outbound client (if present) — not used
- Any polling-result cleanup path that used 202-followed-by-callback — replaced by in-line `GET /instructions` + `POST /result`
- The cost-optimisation *placeholder* — replaced by a working price-based placement path (per-resource `unitPrices` → per-chunk cost → per-consumer node-group masking gated by `ConsumerPolicy`)

### 11.2 From Multi-Cluster-Autoscaler (gRPC Server)

**Reuse as-is:**
- Proto file contract (15 methods defined in `externalgrpc.proto`)
- gRPC server scaffolding and method signatures
- General Liqo-virtual-node concept

**Modify:**
- Replace all in-memory state with `VirtualNodeState` CRD
- Replace Discovery Server with Consumer-Agent-mediated node-group discovery (`GET /local/nodegroups`)
- Replace Node Manager's direct Liqo calls with Consumer-Agent-mediated flow (`POST/DELETE /local/reservations`)
- Add a local HTTP client that talks only to the Consumer Agent (no Broker client, no Broker credentials)
- Add local caching of Consumer Agent responses (serve stale data when the Consumer Agent is temporarily unreachable)
- Add reconciliation loop (VirtualNodeState ↔ actual Liqo virtual nodes)
- Add 500 ms polling of `GET /local/reservations/{id}` bounded by CA's `MaxNodeProvisionDuration`
- Add proper error handling (no `log.Fatalf`)

**Remove:**
- Discovery Server (replaced by the Broker's provider knowledge, relayed via the Consumer Agent)
- Node Manager (responsibilities split: gRPC server manages state, Consumer Agent executes `liqoctl peer` / `liqoctl unpeer`; Provider Agent executes `liqoctl generate peering-user`)
- In-memory node/group maps (replaced by CRDs)
- Direct `liqoctl` subprocess calls from the gRPC server (moved to the two agents per role)
- Hardcoded values (replaced by the `chunk-config` ConfigMap on the Broker, relayed via the Consumer Agent)
- Any direct Broker client code

---

## Summary

This architecture achieves multi-provider cluster autoscaling on top of an **unmodified** Kubernetes Cluster Autoscaler, organised around two principles:

1. **Agent-only outbound communication with the Broker.** Every cross-cluster byte is the response body of a request that a **Consumer Agent** or a **Provider Agent** initiated. Consumer and provider clusters can therefore sit behind NAT or firewalls and still participate, as long as they can reach the Broker's URL.
2. **Agent-centric design with two specialized roles.** On the consumer cluster the gRPC server talks only to its co-located **Consumer Agent** over an in-cluster REST API; the Consumer Agent is the consumer's single point of contact with the Broker and the sole executor of `liqoctl peer` / `liqoctl unpeer`. On the provider cluster the **Provider Agent** is the only component talking to the Broker and the sole executor of `liqoctl generate peering-user`. The two agents share a common codebase — outbound mTLS client, retrier, idempotency cache — but run as two distinct binaries / Deployments, selected at startup via `--role=consumer|provider`.

The Broker runs a `k8s-resource-brokering`-aligned REST API: Provider Agents `POST /advertisements` every 30 s (response piggybacks pending instructions), Consumer Agents `POST /reservations` synchronously (decision is returned inline) and `POST /heartbeat` every 15 s, and both agents `GET /instructions` every 5 s to pick up any pending work. Results (including the peering kubeconfig, which is inlined in `/instructions` responses but never stored in CRD status) flow back through `POST /instructions/{id}/result`. Chunking divides each provider's capacity into fixed units that become Liqo `ResourceSlice`s and ultimately virtual nodes that CA sees as ordinary pool members. Provider selection is the **Broker's** job, not CA's: providers advertise per-resource `unitPrices`, and for any consumer that opts in via a `ConsumerPolicy{placement.type: Price}` the Broker masks that consumer's node-group view to the cheapest priced provider with capacity (cheapest-first greedy) — so CA runs a neutral, price-agnostic expander while the Broker makes the cost decision. All cross-cluster traffic is mTLS, and every reservation-scoped call carries an idempotency key so that retries never cause double-execution.
