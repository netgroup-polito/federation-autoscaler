# Getting Started

This guide walks you from a clean machine to a working federation scale-up.
The federation-autoscaler runs on top of three sibling projects:
Kubernetes Cluster Autoscaler (used unmodified),
[Liqo](https://liqo.io) (multi-cluster peering + virtual nodes), and a
tiny [Resource
Broker](https://github.com/netgroup-polito/k8s-resource-brokering)-style
decision engine packaged in this repo.

**Pick one of three install paths** depending on what you have available:

- **[Option A — Four hosts, real k3s, Ansible-driven](#3-option-a--four-host-real-cluster-install-ansible)** —
  the production-like path. Four Ubuntu VMs, single-node k3s on each,
  everything wired together by an Ansible playbook. ~45 minutes;
  scale-down is fully automatic (Liqo offloading actually works on real
  k3s). **Recommended for anything resembling a real demo or evaluation.**
- **[Option B — Single Kind cluster, dev install](#4-option-b--single-cluster-dev-install)** —
  the broker / agent / gRPC server come up on one local Kind cluster.
  No peering, no scale-up; only useful for inspecting that the deploys
  render and the binaries start.
- **[Option C — Four Kind clusters, full federation in one box](#5-option-c--four-cluster-kind-federation-install)** —
  the e2e suite's topology. Scale-up works end-to-end, but scale-down
  has to be triggered manually because Liqo's data-plane offloading on
  Kind-on-shared-network sometimes traps Pods in `OffloadingBackOff`.
  ~15 minutes once your laptop is set up.

For the deeper architectural background, see [`design.md`](./design.md).

---

## Table of Contents

1. [Prerequisites](#1-prerequisites)
2. [Build the component images](#2-build-the-component-images)
3. [Option A — Four-host real-cluster install (Ansible)](#3-option-a--four-host-real-cluster-install-ansible)
4. [Option B — Single-cluster dev install](#4-option-b--single-cluster-dev-install)
5. [Option C — Four-cluster Kind federation install](#5-option-c--four-cluster-kind-federation-install)
6. [Peer a first provider](#6-peer-a-first-provider)
7. [Watch a scale-up](#7-watch-a-scale-up)
8. [Teardown](#8-teardown)
9. [Troubleshooting](#9-troubleshooting)

---

## 1. Prerequisites

| Tool        | Tested version | Used for |
|-------------|----------------|----------|
| Go          | 1.24 +         | `make docker-build`, running unit tests |
| Docker      | 23 +           | Image builds + Kind backend |
| `kind`      | 0.23 +         | Provisioning local Kubernetes clusters |
| `kubectl`   | 1.30 +         | All cluster interactions |
| `kustomize` | 5.x            | Rendering `config/` overlays |
| `liqoctl`   | v1.1.2         | Liqo install + peering on every cluster |

A single command verifies the four CLI tools the e2e suite expects:

```bash
make setup-test-e2e
```

(`make setup-test-e2e` only checks for binaries — it does not provision
clusters. The Kind clusters come up inside the suite itself; see §4.)

---

## 2. Build the component images

The repo ships one Dockerfile parametrised by `COMPONENT=` (broker / agent /
grpc-server). The agent image additionally bundles a pinned `liqoctl`
binary on `$PATH` so the consumer Peer/Unpeer + provider GenerateKubeconfig
handlers can shell out to it.

```bash
make docker-build                       # builds all three images
# or, scoped:
make docker-build COMPONENT=broker
make docker-build COMPONENT=agent
make docker-build COMPONENT=grpc-server
```

Output: `federation-autoscaler/broker:latest`,
`federation-autoscaler/agent:latest`,
`federation-autoscaler/grpc-server:latest` in your local docker daemon.

---

## 3. Option A — Four-host real-cluster install (Ansible)

The production-like path: four Ubuntu VMs running single-node k3s each,
bootstrapped + deployed by an Ansible playbook. This is what you want for
a real demo or for evaluating federation-autoscaler on hardware you
control. Scale-down is fully automatic because real Liqo on real k3s
offloads Pods successfully (unlike the Kind option in §5).

The full walkthrough — hardware requirements, network ports, tooling
install, inventory setup, the three playbooks — is documented as a
standalone guide:

**→ [`../deploy/ansible/README.md`](../deploy/ansible/README.md)**

Skim that, then jump back to [§6 Peer a first provider](#6-peer-a-first-provider)
and [§7 Watch a scale-up](#7-watch-a-scale-up) to drive the demo flow —
the `kubectl` commands there are kubeconfig-driven and work identically
whether you're pointing at the Ansible-provisioned k3s clusters or the
Kind clusters from §5.

---

## 4. Option B — Single-cluster dev install

Useful when you want to inspect the broker + agent + gRPC server on one
cluster without provisioning the full federation. No peering happens in
this mode; this only validates that the deployment manifests are sane and
that the components come up.

```bash
# 1. spin up a single Kind cluster
kind create cluster --name fa-dev

# 2. install cert-manager (the broker overlay needs it for the PKI chain)
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.18.2/cert-manager.yaml
kubectl --namespace cert-manager wait deployment.apps/cert-manager-webhook \
  --for=condition=Available --timeout=5m

# 3. load the images into the Kind cluster
kind load docker-image federation-autoscaler/broker:latest      --name fa-dev
kind load docker-image federation-autoscaler/agent:latest       --name fa-dev
kind load docker-image federation-autoscaler/grpc-server:latest --name fa-dev

# 4. apply the meta overlay (CRDs + broker + consumer agent + gRPC server)
kustomize build config/default | kubectl apply -f -

# 5. wait for everything to be Ready
kubectl --namespace federation-autoscaler-system wait deployment.apps \
  --all --for=condition=Available --timeout=5m
```

Inspect what you've got:

```bash
kubectl --namespace federation-autoscaler-system get all
kubectl --namespace federation-autoscaler-system get \
  clusteradvertisements,reservations,virtualnodestates
```

You'll see the broker + agent + gRPC server Deployments running, no
ClusterAdvertisements (no provider in this topology), and no
VirtualNodeStates. To exercise peering and scale-up you need at least one
provider cluster — see Option B.

> **Note:** the `agent-config` ConfigMap ships with `REPLACE_ME_*`
> placeholders. The single-cluster install does not actually peer anything,
> so the placeholders don't fire — but a real install must patch them
> (see step 5 below for the convention).

---

## 5. Option C — Four-cluster Kind federation install

The full happy-path topology — central + consumer-1 + provider-1 +
provider-2 — but all four clusters as Kind containers on one host
sharing a docker network. This is what the e2e suite stands up. Useful
for development iteration; less useful for a demo because Liqo's
data-plane offloading on Kind-on-shared-network sometimes traps Pods in
`OffloadingBackOff` and prevents automatic scale-down. For a real demo,
use [Option A](#3-option-a--four-host-real-cluster-install-ansible).

The fastest path is to let the e2e suite do the standup for you:

```bash
make test-e2e
```

That command builds the images, provisions the four Kind clusters, installs
cert-manager + Liqo + CRDs on each, applies the per-role kustomize
overlay, pins the broker Service to a NodePort, stamps each agent's
cluster identity, deploys Cluster Autoscaler with
`--cloud-provider=externalgrpc`, and finally drives the scale-up assertions
in [`test/e2e/`](../test/e2e/).

If you'd like the suite to leave the clusters in place after a successful
(or failed) run for interactive poking:

```bash
KEEP_CLUSTERS=true make test-e2e
```

The four clusters are named `fa-central`, `fa-consumer-1`, `fa-provider-1`,
`fa-provider-2`. Per-cluster kubeconfigs are written to a `fa-e2e-kubeconfigs-*`
directory under `$TMPDIR`; the suite logs the path on stdout.

To switch contexts:

```bash
kind get kubeconfig --name fa-central     > /tmp/central.kubeconfig
kind get kubeconfig --name fa-consumer-1  > /tmp/consumer-1.kubeconfig
kind get kubeconfig --name fa-provider-1  > /tmp/provider-1.kubeconfig
kind get kubeconfig --name fa-provider-2  > /tmp/provider-2.kubeconfig
```

---

## 6. Peer a first provider

With the topology up, watch how a provider registers with the broker. The
provider agent's bootstrap is automatic — it sends `POST /api/v1/advertisements`
every 30 s as soon as it's deployed. To watch:

```bash
KUBECONFIG=/tmp/central.kubeconfig kubectl --namespace federation-autoscaler-system \
  get clusteradvertisements -w
```

You should see a `ClusterAdvertisement` named `fa-provider-1` (and another
for `fa-provider-2`) appear within ~30 s, with `STATUS.AVAILABLE=true` and
non-zero `STATUS.TOTALCHUNKS`.

Behind the scenes (per `docs/design.md §8.1`):

1. The provider agent's snapshot package walks local nodes via the
   Kubernetes API.
2. It computes per-chunk capacity using the `chunk-config` ConfigMap
   defaults (2 cpu / 4 Gi per standard chunk).
3. It POSTs that to the broker over mTLS — the client cert's CN is what
   identifies the cluster (`docs/design.md §7.0 — Cluster identity`).
4. The broker's `ClusterAdvertisement` reconciler flips `Available=true`
   once it sees a fresh `LastSeen`.

Peering itself happens **on-demand**, not at boot — only when the gRPC
server actually issues a reservation does the broker queue a
`ProviderInstruction{GenerateKubeconfig}`. Move on to §6 to trigger one.

---

## 7. Watch a scale-up

Schedule a synthetic workload on consumer-1 with per-Pod requests too big
for the consumer cluster's local nodes. Two specifics are load-bearing:

- `nodeSelector: liqo.io/type=virtual-node` — pins the workload to
  federation capacity. Without this the Pods would happily fit on the
  consumer's local worker (Kind workers inherit the host's CPU budget),
  CA would never see Pending pods, and no scale-up would trigger.
- `tolerations` for `virtual-node.liqo.io/not-allowed:NoExecute` —
  Liqo stamps that taint on every materialised VirtualNode; without
  the toleration the scheduler refuses to bind even after CA spins
  up the node.

```bash
KUBECONFIG=/tmp/consumer-1.kubeconfig kubectl apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: federation-scaleup-driver
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: federation-scaleup-driver
  template:
    metadata:
      labels:
        app.kubernetes.io/name: federation-scaleup-driver
    spec:
      nodeSelector:
        liqo.io/type: virtual-node
      tolerations:
      - key: virtual-node.liqo.io/not-allowed
        operator: Exists
        effect: NoExecute
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
        resources:
          requests:
            cpu: 1500m
            memory: 1Gi
        command: ["/pause"]
EOF
```

Watch the Reservation phase progress on the central cluster:

```bash
KUBECONFIG=/tmp/central.kubeconfig kubectl --namespace federation-autoscaler-system \
  get reservations -w -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,PROVIDER:.spec.providerClusterId
```

Within ~5 minutes you should see a new Reservation cycle through
`Pending → GeneratingKubeconfig → KubeconfigReady → Peering → Peered`. The
big step is `Peering`: `liqoctl peer` brings up the WireGuard tunnel,
exchanges Identity CRs, and waits for Liqo's controller-manager to
materialise the `VirtualNode` — this routinely takes 3–5 minutes on
constrained hosts. Each phase corresponds to a step in `docs/design.md §8.2`:

| Phase                  | What's happening                                                                   |
|------------------------|------------------------------------------------------------------------------------|
| `Pending`              | Broker accepted the `POST /api/v1/reservations` synchronously                      |
| `GeneratingKubeconfig` | Broker queued `ProviderInstruction{GenerateKubeconfig}` for the chosen provider    |
| `KubeconfigReady`      | Provider agent ran `liqoctl generate peering-user` and posted the kubeconfig back  |
| `Peering`              | Consumer agent ran `liqoctl peer` and created the Liqo `ResourceSlice`             |
| `Peered`               | Liqo materialised the `VirtualNode`; CA can now schedule onto it                   |

Watch the Liqo VirtualNode appear on the consumer. Liqo creates the CR
in a per-provider tenant namespace (`liqo-tenant-<provider-id>`), not
the federation-autoscaler namespace — so query across all namespaces:

```bash
KUBECONFIG=/tmp/consumer-1.kubeconfig kubectl get virtualnodes.offloading.liqo.io --all-namespaces -w
```

Once the VirtualNode reports `status.conditions[type=Node].status=Running`,
the v1.Node Liqo materialises shows up under `kubectl get nodes` with
the `liqo.io/type=virtual-node` label, and the kube-scheduler binds your
driver Pods to it:

```bash
KUBECONFIG=/tmp/consumer-1.kubeconfig kubectl get pods \
  -l app.kubernetes.io/name=federation-scaleup-driver -w \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,NODE:.spec.nodeName
```

You'll see each Pod's `NODE` column populate with the provider's name
(e.g. `provider-1`). That's the federation-autoscaler success signal:
CA → broker → agent → Liqo produced a remote virtual node, and the
scheduler bound the workload to it.

**Note on `OffloadingBackOff`.** Whether the Pod then transitions to
`Running` depends on Liqo's data-plane offloading actually pushing the
shadow Pod across the WireGuard tunnel and starting it on the provider.
On constrained Kind-on-shared-network setups this step sometimes hits
`OffloadingBackOff` because of Liqo CNI/IPAM quirks — that's an upstream
Liqo concern, separate from federation-autoscaler. The Pod being
*scheduled onto the federation virtual node* is the strongest signal
the federation-autoscaler chain is healthy end-to-end.

---

## 8. Teardown

Drop the synthetic workload, then either delete the Reservation manually
(which exercises the scale-down path — `docs/design.md §8.3`) or tear the
whole topology down:

```bash
# manual scale-down (exercises Unpeer / Cleanup):
KUBECONFIG=/tmp/consumer-1.kubeconfig kubectl delete deployment \
  federation-scaleup-driver -n default
KUBECONFIG=/tmp/central.kubeconfig kubectl --namespace federation-autoscaler-system \
  delete reservations --all

# full topology teardown:
make cleanup-test-e2e
```

---

## 9. Troubleshooting

| Symptom | Where to look |
|---|---|
| Broker pod CrashLoopBackOff with `unable to load server keypair` | `kubectl -n federation-autoscaler-system describe secret broker-server-cert` — cert-manager hasn't issued the keypair yet; wait 30 s or check `cmctl status certificate broker-server`. |
| Agent pod `unable to validate certificate: x509: certificate signed by unknown authority` | The `agent-client-cert` Secret was issued by a different CA than the broker's. In a multi-cluster install you must wire cert-manager `trust-manager` (or an external Vault Issuer) so all four clusters chain to the same root. |
| Reservation stuck at `GeneratingKubeconfig` | The provider agent isn't polling. Check `kubectl logs -n federation-autoscaler-system deploy/agent` on the provider cluster; common cause is `BROKER_URL` not pointing at the central cluster's NodePort. |
| Reservation reaches `Peering` then flips to `Failed` | `liqoctl peer` failed on the consumer agent. Tail `kubectl logs -n federation-autoscaler-system deploy/agent --tail=200 -f` on the consumer cluster for the underlying stderr. |
| `VirtualNode` exists but Pods stay `Pending` | The chunk shape doesn't match the Pod's requests, or the `NamespaceOffloading` is missing — `kubectl get namespaceoffloadings.offloading.liqo.io -A` should list one named `no-<reservation-id>`. |

For tracing through the entire flow with verbose logs:

```bash
KUBECONFIG=/tmp/central.kubeconfig kubectl --namespace federation-autoscaler-system \
  logs deploy/broker -f --tail=200 &
KUBECONFIG=/tmp/consumer-1.kubeconfig kubectl --namespace federation-autoscaler-system \
  logs deploy/agent -f --tail=200 &
KUBECONFIG=/tmp/consumer-1.kubeconfig kubectl --namespace federation-autoscaler-system \
  logs deploy/grpc-server -f --tail=200 &
```

For a one-shot end-to-end trace, `make test-e2e` runs everything above
non-interactively and tears the topology down on completion.

---

## What next?

- [`design.md`](./design.md) — the as-built architecture spec, with
  per-section "Implemented in:" footers pointing at the canonical Go
  packages.
- [`diagrams/`](./diagrams/) — the registration / scale-up / scale-down
  flow charts.
- [README "Status" table](../README.md) — current alpha-feature coverage.
