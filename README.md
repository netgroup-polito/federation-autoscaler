# federation-autoscaler

**A Kubernetes Cluster Autoscaler extension that dynamically borrows compute capacity from multiple existing Kubernetes clusters across heterogeneous providers — cloud, edge, and on-premise — and exposes them to the local scheduler as virtual nodes via Liqo.**

<!-- Replace the placeholders once CI / releases exist -->
<!-- ![build](https://img.shields.io/github/actions/workflow/status/netgroup-polito/federation-autoscaler/ci.yml) -->
<!-- ![go](https://img.shields.io/github/go-mod/go-version/netgroup-polito/federation-autoscaler) -->
<!-- ![license](https://img.shields.io/github/license/netgroup-polito/federation-autoscaler) -->

**Status:** alpha. The happy-path scale-up works end-to-end across a four-Kind topology; v1 limitations are documented under [Limitations](#limitations-v1).

---

## What it does

- Runs vanilla **Kubernetes Cluster Autoscaler** on the consumer cluster — no CA source-code changes.
- A small **gRPC Server** implements CA's `externalgrpc` contract and asks a local **Consumer Agent** for node groups and reservations.
- The **Consumer Agent** (on consumer clusters) and the **Provider Agent** (on provider clusters) talk — **agent-initiated only, over mTLS** — to a central **Resource Broker** that decides which provider cluster should donate capacity.
- **Liqo** then peers the two clusters and exposes the remote capacity as virtual nodes that CA (and the scheduler) treat like any other Node.

Works from NATed / firewalled clusters: only the Broker needs a public endpoint; consumers and providers need outbound egress only.

---

## Status

| Area | Status | Notes / source |
|---|---|---|
| Broker REST API (mTLS, all endpoints in `docs/design.md §7.3.1–§7.3.7, §7.3.9`) | ✅ alpha | `internal/broker/api/` |
| Broker CRDs (`ClusterAdvertisement`, `Reservation`, `ProviderInstruction`, `ReservationInstruction`) + reconcilers | ✅ alpha | `api/broker/`, `api/autoscaling/`, `internal/controller/{broker,autoscaling}/` |
| Reservation phase machine (`Pending → … → Peered → Released`) | ✅ alpha | `internal/controller/broker/reservation_controller.go` |
| Shared agent core (mTLS HTTP client, 5 s poller, health probe) | ✅ alpha | `internal/agent/{client,poller,health}/` |
| Provider Agent (advertisement + `GenerateKubeconfig` / `Cleanup` / `Reconcile`) | ✅ alpha | `internal/agent/provider/` |
| Consumer Agent (heartbeat + loopback REST + `Peer` / `Unpeer` / `Cleanup` / `Reconcile`) | ✅ alpha | `internal/agent/consumer/` |
| gRPC Server (14 of 15 `externalgrpc` RPCs implemented) | ✅ alpha | `internal/grpcserver/` — `NodeGroupGetOptions` returns Unimplemented (proto permits it) |
| `VirtualNodeState` reconciler — projects the consumer `v1.Node` status (Ready, allocatable, `providerID`) onto the CR | ✅ alpha | `internal/controller/autoscaling/virtualnodestate_controller.go` (correlates by the cluster-scoped `v1.Node` named after the provider's Liqo cluster ID) |
| Per-binary kustomize overlays + cert-manager PKI + scoped RBAC | ✅ alpha | `config/broker/`, `config/agent/{base,consumer,provider}/`, `config/grpc-server/`, `config/default/` (meta) |
| Multi-cluster end-to-end suite (4-Kind topology) | ✅ alpha | `test/e2e/` (build-tagged `e2e`, run via `make test-e2e`) |
| Partial release (`DELETE /reservations/{id}?chunks=N`) | ⏳ deferred to v2 | `internal/broker/api/reservation.go:458` — v1 always releases the whole reservation |
| Diff-apply on `Reconcile` instruction (broker adopts agent snapshot) | ⏳ deferred to v2 | v1 agents report state; the broker does not reconcile drift back |
| Per-endpoint role gating (provider-only vs consumer-only) | ⏳ deferred | mTLS CN-vs-payload cross-check is in place; the dedicated role attribute on the cert is not |
| Dedicated 2 rps bucket on `POST /reservations` | ⏳ deferred | Per-cluster bucket is shared with `/instructions` today |
| 7-day terminal-reservation GC | ⏳ deferred | Terminal CRs persist until manually deleted |
| `GET /api/v1/advertisements/{clusterId}` + `GET /api/v1/reservations/{id}` | ⏳ deferred | Read-only convenience endpoints not yet wired |

---

## Architecture at a glance

![Architecture](docs/diagrams/architecture-diagram.png)

| Channel | Initiator | Transport |
| --- | --- | --- |
| CA ↔ gRPC Server | CA | gRPC, in-cluster |
| gRPC Server ↔ Consumer Agent | gRPC Server | HTTP, in-cluster |
| Consumer Agent ↔ Broker | **Consumer Agent only** | HTTPS + mTLS — 5 s polling + synchronous POSTs (heartbeat, reservations) |
| Provider Agent ↔ Broker | **Provider Agent only** | HTTPS + mTLS — 5 s polling + synchronous POSTs (advertisements every 30 s) |
| Broker → any Agent | *never happens* | — |

Full design, CRDs, API contracts, and execution flows: **[docs/design.md](docs/design.md)** (as-built — every section has an `Implemented in:` footer pointing at the canonical Go package).

---

## Repository layout

```
federation-autoscaler/
├── api/            # CRD Go types (multi-group kubebuilder layout)
├── cmd/            # broker, agent, grpc-server entrypoints
├── config/         # kustomize overlays (broker, agent/{base,consumer,provider}, grpc-server, default meta)
├── internal/       # controllers, broker REST API, agent core, gRPC server
├── docs/           # design.md (as-built), getting-started.md, diagrams/
├── hack/           # development scripts
├── test/
│   ├── e2e/        # multi-cluster e2e suite (kind/, bootstrap/, scenario/) — build tag `e2e`
│   └── utils/
├── Dockerfile      # multi-stage, parametrised by COMPONENT=; agent image bundles liqoctl
├── Makefile        # component-driven (COMPONENT=broker|agent|grpc-server)
└── PROJECT         # kubebuilder metadata
```

---

## Quick start

Two paths — pick one:

```bash
# 1. Run the full happy-path on a 4-Kind topology (boots clusters, deploys
#    everything, drives a synthetic scale-up, tears down):
make test-e2e

# 2. Inspect the components on a single cluster without peering:
kind create cluster --name fa-dev
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.18.2/cert-manager.yaml
make docker-build
kind load docker-image federation-autoscaler/broker:latest      --name fa-dev
kind load docker-image federation-autoscaler/agent:latest       --name fa-dev
kind load docker-image federation-autoscaler/grpc-server:latest --name fa-dev
kustomize build config/default | kubectl apply -f -
```

The interactive tutorial — including how to peer a first provider and watch a scale-up — is in **[docs/getting-started.md](docs/getting-started.md)**.

---

## Documentation

- **[docs/getting-started.md](docs/getting-started.md)** — install + tutorial (prereqs, build images, peer a provider, watch a scale-up, troubleshoot).
- **[docs/design.md](docs/design.md)** — full architectural proposal (v3.1, as-built) with `Implemented in:` footers.
- **[docs/diagrams/](docs/diagrams/)** — Mermaid sources + PNG renderings of the architecture / registration / scale-up / scale-down flows.

---

## Limitations (v1)

The pieces flagged ⏳ in the [Status](#status) table above are documented as v2 deliverables — none of them block the happy path. The two with the most impact for production deployments:

- **Full-release-only scale-down.** Calling `NodeGroupDeleteNodes` on a single virtual node in a multi-chunk reservation today releases the entire reservation, not just that chunk. Cluster Autoscaler retries are safe (idempotent), but batched deletes across chunks of the same reservation should be issued together.
- **No drift-recovery diff-apply.** The broker emits `Reconcile` instructions on startup, and agents return a snapshot, but the broker doesn't yet adopt agent-side state it didn't expect. A clean broker restart works; recovering from a long broker outage with stale CRs may need a manual clean.

---

## Contributing

Issues and pull requests are welcome. We follow the standard *fork → branch → PR* workflow. Please run `make fmt lint test` before opening a pull request.

---

## Reference

Based on the paper *"Dynamic Multi-Provider Cluster Autoscaling For The Computing Continuum"*, ACM SAC 2025.

---

## License

[Apache License 2.0](LICENSE) — kubebuilder default.
