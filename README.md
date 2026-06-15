# federation-autoscaler

**A Kubernetes Cluster Autoscaler extension that dynamically borrows compute capacity from multiple existing Kubernetes clusters across heterogeneous providers — cloud, edge, and on-premise — and exposes them to the local scheduler as virtual nodes via Liqo.**

<!-- Replace the placeholders once CI / releases exist -->
<!-- ![build](https://img.shields.io/github/actions/workflow/status/netgroup-polito/federation-autoscaler/ci.yml) -->
<!-- ![go](https://img.shields.io/github/go-mod/go-version/netgroup-polito/federation-autoscaler) -->
<!-- ![license](https://img.shields.io/github/license/netgroup-polito/federation-autoscaler) -->

---

## What it does

- Runs vanilla **Kubernetes Cluster Autoscaler** on the consumer cluster — no CA source-code changes.
- A small **gRPC Server** implements CA's `externalgrpc` contract and asks a local **Consumer Agent** for node groups and reservations.
- The **Consumer Agent** (on consumer clusters) and the **Provider Agent** (on provider clusters) talk — **agent-initiated only, over mTLS** — to a central **Resource Broker** that decides which provider cluster should donate capacity.
- **Liqo** then peers the two clusters and exposes the remote capacity as virtual nodes that CA (and the scheduler) treat like any other Node.

Works from NATed / firewalled clusters: only the Broker needs a public endpoint; consumers and providers need outbound egress only.

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
├── deploy/
│   └── ansible/    # 4-host k3s demo install: playbooks, roles, demo-up.sh, burst-workload sample
├── docs/           # design.md (as-built), diagrams/
├── hack/           # development scripts
├── internal/       # controllers, broker REST API, agent core, gRPC server
├── test/
│   ├── e2e/        # multi-cluster e2e suite (kind/, bootstrap/, scenario/) — build tag `e2e`
│   └── utils/
├── Dockerfile      # multi-stage, parametrised by COMPONENT=; agent image bundles liqoctl
├── Makefile        # component-driven (COMPONENT=broker|agent|grpc-server)
└── PROJECT         # kubebuilder metadata
```

---

## Quick start

You need four Ubuntu 22.04/24.04 VMs on the same network — 1 central, 1 consumer, 2 providers. Then, from a control host:

```bash
# 1. Bring up the whole demo in one command — installs tooling, then k3s,
#    cert-manager, Liqo, broker, agents, grpc-server and Cluster Autoscaler
#    on every VM:
curl -fsSL https://raw.githubusercontent.com/netgroup-polito/federation-autoscaler/main/deploy/ansible/scripts/demo-up.sh -o demo-up.sh
chmod +x demo-up.sh
./demo-up.sh --central <ip> --consumers <ip> --providers <ip>,<ip>

# 2. Watch the broker's state live in a browser — its read-only web dashboard
#    (advertisements, reservations, instructions, chunk capacity, consumers):
#    open http://<central-ip>:30444/

# 3. Scale UP — apply the burst workload; the replicas that don't fit on the
#    consumer's own node spill onto virtual nodes borrowed from the providers:
kubectl --kubeconfig ~/.kube/consumer-1.yaml apply -f ~/federation-autoscaler/deploy/ansible/samples/burst-workload.yaml

# 4. Scale DOWN — delete it; Cluster Autoscaler releases the borrowed nodes:
kubectl --kubeconfig ~/.kube/consumer-1.yaml delete -f ~/federation-autoscaler/deploy/ansible/samples/burst-workload.yaml
```

For the detailed setup — hardware and network requirements, the playbook-by-playbook manual install, tuning variables, troubleshooting — see **[deploy/ansible/README.md](deploy/ansible/README.md)**.

---

## Documentation

- **[deploy/ansible/README.md](deploy/ansible/README.md)** — install + demo guide (hardware/network requirements, the four playbooks step by step, running the demo, troubleshooting).
- **[docs/design.md](docs/design.md)** — full architectural proposal (v3.1, as-built) with `Implemented in:` footers.
- **[docs/diagrams/](docs/diagrams/)** — Mermaid sources + PNG renderings of the architecture / registration / scale-up / scale-down flows.

---

## Contributing

Issues and pull requests are welcome. We follow the standard *fork → branch → PR* workflow. Please run `make fmt lint test` before opening a pull request.

---

## Reference

Based on the paper *"Dynamic Multi-Provider Cluster Autoscaling For The Computing Continuum"*, ACM SAC 2025.

---

## License

[Apache License 2.0](LICENSE) — kubebuilder default.
