# federation-autoscaler — Ansible deployment

Bootstraps a four-cluster federation-autoscaler topology on real Ubuntu hosts
running single-node **k3s**. Mirrors the `test/e2e/bootstrap/` Go helpers
that drive the in-tree e2e suite, so the production install is a literal
subset of what's already verified end-to-end.

If you're new to this project: start here, follow the sections top-to-bottom,
and you'll have a working 4-host federation in roughly 45 minutes.

## What you'll end up with

| Host         | Role                                                                                                  |
| ------------ | ----------------------------------------------------------------------------------------------------- |
| `central`    | k3s + cert-manager + Liqo + federation-autoscaler CRDs + **broker**                                   |
| `consumer-1` | k3s + cert-manager + Liqo + CRDs + **consumer agent** + **grpc-server** + Cluster Autoscaler          |
| `provider-1` | k3s + cert-manager + Liqo + CRDs + **provider agent**                                                 |
| `provider-2` | k3s + cert-manager + Liqo + CRDs + **provider agent**                                                 |

Add more `consumer-*` / `provider-*` hosts later by extending the inventory.

## Hardware requirements

Per-VM minimums sized so the demo runs comfortably without anyone running out
of resources mid-narration. Sizing is dominated by what each cluster hosts
*beyond* k3s itself — Liqo control plane (~700 MB), federation-autoscaler
binaries, and on providers the shadow workload + WireGuard gateway.

| VM           | vCPU | RAM   | Disk     | Why                                                                                                     |
| ------------ | ---- | ----- | -------- | ------------------------------------------------------------------------------------------------------- |
| `central`    | 2    | 2 GB  | 20 GB    | Broker is tiny, but k3s + cert-manager + Liqo control plane still need this floor.                      |
| `consumer-1` | 2    | 4 GB  | 20 GB    | Agent + gRPC server + Cluster Autoscaler + Liqo control plane + WireGuard client + workload Pods.       |
| `provider-1` | 4    | 8 GB  | 30 GB    | Liqo gateway server + the offloaded shadow workload. At 4 vCPU / 8 GB it advertises **one** "standard" chunk (2 vCPU / 4 GiB) — memory-bound: ~7.75 GiB allocatable ÷ 4 GiB rounds down to 1. |
| `provider-2` | 4    | 8 GB  | 30 GB    | Same — the second provider gives the broker a placement choice (and is the cheaper one in the price scenario).                                 |

**OS**: Ubuntu 22.04 or 24.04, x86_64.

If you'd rather have homogeneous boxes, 4 vCPU / 8 GB / 30 GB on **all four**
is fine and removes the asymmetric-sizing worry.

**Tested configuration** (what the demo — including the price-policy scenario —
was validated on): all four VMs **Ubuntu Server 24.04**, x86_64;
`central` and `consumer-1` at **4 vCPU / 4 GB RAM / 20 GB disk**, `provider-1`
and `provider-2` at **4 vCPU / 8 GB RAM / 30 GB disk**. At that provider size
each advertises exactly **one** standard chunk, which is why the 7-replica
`burst-workload` (two chunks of overflow) fills both providers while the
5-replica `price-workload` (one chunk) lands entirely on the single cheapest
provider.

## Network requirements

All four VMs need to be on the same private network with reachability to each
other (a single VLAN, a flat cloud VPC, or four VMs on the same hypervisor
all work). Open the following ports between them:

| Port range          | Protocol | Used by                              |
| ------------------- | -------- | ------------------------------------ |
| 22                  | TCP      | SSH from your laptop                  |
| 6443                | TCP      | k3s apiserver (used by `kubectl`)     |
| 30443               | TCP      | Broker NodePort (agents → broker)     |
| 30444               | TCP      | Broker dashboard NodePort (browser)   |
| 30445               | TCP      | Agent config console NodePort (browser) |
| 80                  | TCP      | Liqo dashboard Ingress (consumer)     |
| 30000–32767         | UDP      | Liqo WireGuard gateway                |

Outbound HTTPS from every VM is required during the bootstrap (k3s installer,
cert-manager YAML, Docker Hub image pulls, Liqo CRD downloads).

## Control host (your laptop)

The Ansible playbook runs from your laptop. It connects to each VM over SSH
and runs `kubectl` / `liqoctl` against the four clusters locally with the
kubeconfigs it fetches during bootstrap.

### One-time tooling install

```bash
# Ansible
sudo apt-get update
sudo apt-get install -y ansible-core

# kubectl 1.32
curl -LO "https://dl.k8s.io/release/v1.32.5/bin/linux/amd64/kubectl"
sudo install -m0755 kubectl /usr/local/bin/kubectl
rm kubectl

# kustomize v5
curl -s "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh" | bash
sudo install -m0755 kustomize /usr/local/bin/kustomize
rm kustomize

# liqoctl v1.1.2 (must match the version bundled in the agent image)
curl -fsSL "https://github.com/liqotech/liqo/releases/download/v1.1.2/liqoctl-linux-amd64.tar.gz" | tar -xz -C /tmp
sudo install -m0755 /tmp/liqoctl /usr/local/bin/liqoctl
```

> **Note**: you do **not** need Docker on the control host. The
> federation-autoscaler images are published to Docker Hub by the project
> maintainers — the playbook pulls them directly from there. See "Maintainer
> notes" at the bottom of this README for the publish flow.

### SSH key to all four VMs

```bash
ssh-copy-id ubuntu@<central-ip>
ssh-copy-id ubuntu@<consumer-1-ip>
ssh-copy-id ubuntu@<provider-1-ip>
ssh-copy-id ubuntu@<provider-2-ip>

# Smoke-test SSH + passwordless sudo on every VM:
for ip in <central-ip> <consumer-1-ip> <provider-1-ip> <provider-2-ip>; do
  ssh ubuntu@$ip "hostname && sudo -n true && echo sudo-ok" || echo "FAIL $ip"
done
```

All four hosts should print their hostname + `sudo-ok`. If sudo asks for a
password, configure passwordless sudo for the deploy user on the VMs:
`echo 'ubuntu ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ubuntu-nopasswd`.

### Clone the repo

```bash
git clone https://github.com/netgroup-polito/federation-autoscaler ~/federation-autoscaler
cd ~/federation-autoscaler
```

## Step 1 — Fill in the inventory

```bash
cd deploy/ansible
cp inventory.yaml.example inventory.yaml
$EDITOR inventory.yaml
```

Replace the four placeholder `ansible_host:` IPs with your VMs' private IPs.
If your SSH user isn't `ubuntu`, change `ansible_user:` at the top of the
file. Leave the Pod/Service CIDRs as-is unless they collide with something
on your private network (they must not overlap each other).

Smoke-test that Ansible can reach all four hosts:

```bash
ansible -i inventory.yaml all -m ping
```

Every host should reply with `pong`.

## Step 2 — Bootstrap (k3s + cert-manager + Liqo + CRDs)

```bash
ansible-playbook -i inventory.yaml playbooks/01-bootstrap.yaml
```

Takes ~10–15 min on the first run (k3s install + image pulls). When it
finishes you'll have four kubeconfigs on your laptop at
`~/.kube/{central,consumer-1,provider-1,provider-2}.yaml`.

Sanity check:

```bash
for c in central consumer-1 provider-1 provider-2; do
  echo "--- $c ---"
  kubectl --kubeconfig ~/.kube/$c.yaml get nodes
done
```

Each should print one `Ready` node.

## Step 3 — Deploy federation-autoscaler

```bash
ansible-playbook -i inventory.yaml playbooks/02-deploy.yaml

# Deploy a specific image tag (broker/agent/grpc-server) without editing files:
ansible-playbook -i inventory.yaml playbooks/02-deploy.yaml -e fa_tag=v0.X.Y
```

Takes ~5–8 min. Brings up the broker on central, agents on consumers and
providers, gRPC server on consumer, Cluster Autoscaler on consumer. Mirrors
the broker's PKI to every agent cluster. Stamps each agent's TLS cert with
its inventory cluster_id.

The federation-autoscaler image tag comes from `fa_tag` in
`group_vars/all.yaml`. Override it per run with `-e fa_tag=<tag>` (above), or —
if you used the one-shot script — with `demo-up.sh --tag <tag>`. The override
flips all three images (broker, agent, grpc-server) at once.

## Step 4 — Verify

```bash
ansible-playbook -i inventory.yaml playbooks/03-verify.yaml
```

Prints an OK banner per check. If a Deployment isn't Available it tells
you which one. After this returns green, your federation is ready to demo.

## Step 5 — Run the demo

Watch the broker's state live in a browser: it serves a read-only web dashboard
(advertisements, reservations, the instruction phase machine, chunk capacity,
registered consumers — plus each provider's **Cost/chunk** and each consumer's
**Policy**) at `http://<central-ip>:30444/`. It refreshes every couple of
seconds and needs no login — keep it on a trusted network, as it is
unauthenticated.

Each consumer and provider cluster also serves a small **config console** — a
plain-HTTP web UI (no login) at `http://<cluster-ip>:30445/` for setting that
cluster's federation knobs from the browser instead of `kubectl apply`-ing YAML:
the **consumer** console sets the placement policy (Standard / Price / Eco /
Latency) and applies/deletes the demo workload; the **provider** console sets
unit prices, advertised CPU/RAM capacity, and the renewable flag. Both consoles
also show each cluster's **auto-discovered location** read-only (location is no
longer operator-set — it is geolocated from the node IP). The console writes the
same `ConsumerPolicy` / `agent-prices` / `agent-capacity` / `agent-renewable`
resources the samples do, so changes take effect on the next heartbeat (~15 s) /
advertisement (~30 s). It is unauthenticated AND write-capable — keep it on a
trusted network (demo-grade only).

The consumer cluster also runs the [Liqo dashboard](https://github.com/ArubaKube/liqo-dashboard)
(peerings, virtual nodes, offloaded pods), installed by `02-deploy` with image
tag `main`. It is served via the cluster's Traefik Ingress on host
`liqo-dashboard.local`; add `<consumer-ip> liqo-dashboard.local` to your
machine's hosts file, then open `http://liqo-dashboard.local`.

There are two scenarios, both driven by a single workload Deployment — the only
manual action. Run **A** first; **B** layers broker-driven, price-based
placement on top with two extra opt-in steps.

### Scenario A — No policy (default placement)

Out-of-the-box behaviour: no `ConsumerPolicy`, no provider prices. The broker
hands the Cluster Autoscaler **every** available provider, and CA picks which to
grow with its neutral `least-waste` expander — price plays no part. The
7-replica burst workload overflows ~4 Pods = two chunks, so it fills **both**
providers (one chunk each).

```bash
kubectl --kubeconfig ~/.kube/consumer-1.yaml apply  -f samples/burst-workload.yaml   # scale UP
kubectl --kubeconfig ~/.kube/consumer-1.yaml delete -f samples/burst-workload.yaml   # scale DOWN
```

What to expect on these real k3s VMs:

- Pods reach `Running` on the providers: the `fa_consumer` role stamps a
  Liqo `NamespaceOffloading` for the `default` namespace, so Liqo reflects
  the shadow Pods across the WireGuard tunnel and they run on the provider
  clusters. (This is **not** k3s-specific — Liqo offloads Pods to `Running`
  on Kind too, provided the workload namespace has a `NamespaceOffloading`;
  the in-tree e2e currently omits that step and so only asserts scheduling.)
- Scale-down is **fully automatic**: delete the workload, wait
  `scale_down_unneeded_time` (1 min by default — see
  `group_vars/consumers.yaml`), and Cluster Autoscaler fires
  `NodeGroupDeleteNodes` on its own. No manual `kubectl delete reservation`
  needed.

**Proves:** the end-to-end borrow → peer → schedule → release loop across
multiple providers.

### Scenario B — Price-based placement (the broker picks the cheapest)

Here the **broker** chooses the provider — the cheapest one — and CA never sees
a price. Two manual opt-in steps, then a workload sized to a single chunk so the
choice is unambiguous. (If Scenario A is still up, scale it down first.)

1. Declare the consumer's policy (price-preferring) on the consumer:
   ```bash
   kubectl --kubeconfig ~/.kube/consumer-1.yaml apply -f samples/consumer-policy.yaml
   ```
   Within ~15 s (next heartbeat) the dashboard's `consumer-1` row flips to
   `Policy: Price`.

2. Set each provider's prices — provider-2 cheaper than provider-1:
   ```bash
   kubectl --kubeconfig ~/.kube/provider-1.yaml apply -f samples/provider-1-prices.yaml
   kubectl --kubeconfig ~/.kube/provider-2.yaml apply -f samples/provider-2-prices.yaml
   ```
   Within ~30 s (next advertisement) the dashboard shows `Cost/chunk` ≈ **0.116**
   for provider-1 and **0.078** for provider-2.

3. Drive the scale-up with the 5-replica price workload (one chunk of overflow):
   ```bash
   kubectl --kubeconfig ~/.kube/consumer-1.yaml apply  -f samples/price-workload.yaml   # scale UP
   kubectl --kubeconfig ~/.kube/consumer-1.yaml delete -f samples/price-workload.yaml   # scale DOWN
   ```

What to expect: the overflow Pods run on **provider-2 only** (the cheaper one);
provider-1 stays at `reserved=0`. Confirm on the dashboard's Reservations table,
or:

```bash
kubectl --kubeconfig ~/.kube/central.yaml -n federation-autoscaler-system \
  get reservation -o custom-columns=PROVIDER:.spec.providerClusterId,PHASE:.status.phase
```

Flip it live: make provider-2 the *dearer* one (swap the two files' numbers and
re-apply) and the next scale-up switches to provider-1 — no redeploy, CA
untouched. Scale-down is automatic, exactly as in Scenario A.

**Proves:** the broker — not CA — selects the provider purely on advertised
price; the consumer opts in per-policy; prices change live.

> Return to Scenario A by removing the policy:
> `kubectl --kubeconfig ~/.kube/consumer-1.yaml delete -f samples/consumer-policy.yaml`

### Customizing advertised capacity (optional)

By default a provider advertises **100 % of its allocatable** capacity. The
provider-cluster admin can donate *less* — per resource — by applying an
`agent-capacity` ConfigMap, the same manual, post-deploy gesture as setting
prices (the `fa_provider` role does **not** stamp it):

```bash
kubectl --kubeconfig ~/.kube/provider-1.yaml apply -f samples/provider-capacity.yaml
```

`capacity.yaml` is a map of resource → integer percent. A value in `(0,100)`
advertises that fraction of the resource's allocatable; `100`, anything `>100`,
anything `<=0`, or an unlisted resource advertises the **full** allocatable. The
sample caps memory to `50` and leaves CPU at `100`. Within ~30 s (next
advertisement) the broker dashboard's **Custom** column shows `mem 50%` for that
provider, and — because chunks are derived from the (now-reduced) advertised
capacity — a memory-bound provider's `Total` chunk count drops accordingly. The
file is re-read every cycle, so the cap changes live with no restart; clear it
with `kubectl patch configmap agent-capacity ... ` or by re-applying an empty
`capacity.yaml` to return to full capacity.

## Teardown

```bash
ansible-playbook -i inventory.yaml playbooks/04-teardown.yaml
```

Wipes k3s on every host (`k3s-uninstall.sh`), which removes all cluster
state in one shot — Liqo peerings/tenants/authentication/offloading,
cert-manager, the federation-autoscaler namespace + CRDs, and every
workload — then deletes the control-host kubeconfigs so a later
`01-bootstrap` re-fetches them fresh with the current inventory IPs.

It's fully hands-off and always succeeds: it never runs `liqoctl`, so
there is nothing to get stuck on. (Liqo's graceful uninstall refuses to
proceed while any peering/offloading remains, which proved brittle after
an interrupted run or a VM IP change — hence the wipe.) The full clean
cycle is:

```
04-teardown → 01-bootstrap → 02-deploy → 03-verify
```

> **Destructive by design:** this drops the entire k3s node, including any
> PersistentVolumes and host iptables rules. That's exactly what we want on
> the demo VMs — do **not** point it at a k3s cluster you need to keep.

## Where the variables live

- `group_vars/all.yaml` — image registry/tag (`fa_registry` / `fa_tag`),
  software versions, broker exposure (NodePort + central IP). Override the
  image tag per run with `-e fa_tag=<tag>` or `demo-up.sh --tag <tag>`.
- `group_vars/central.yaml` — broker-specific knobs.
- `group_vars/consumers.yaml` — consumer agent + Cluster Autoscaler
  tuning, including the **`scale_down_unneeded_time: 1m`** demo-fast
  knob (bump to `10m` for production).
- `group_vars/providers.yaml` — provider agent knobs.
- `inventory.yaml` (your copy of `inventory.yaml.example`) — the four VMs
  with their private IPs, cluster_ids, and Pod/Service CIDRs.

## Maintainer notes — publishing new images

Users of this playbook pull images from the project-maintained Docker Hub
repos (`docker.io/kazem26/federation-autoscaler-{broker,agent,grpc-server}`).
You do **not** need to do this yourself unless you're cutting a new release.

When the maintainer cuts a release:

```bash
cd ~/federation-autoscaler
docker login -u kazem26
make docker-push REGISTRY=docker.io/kazem26 TAG=v0.X.Y

# Make sure the three repos stay public on hub.docker.com so users can
# pull without authentication.
```

Then make the playbook pull the new version — either bump `fa_tag` in
`group_vars/all.yaml` to make it the new default, or select it per run without
editing anything via `demo-up.sh --tag v0.X.Y` (or `ansible-playbook …
playbooks/02-deploy.yaml -e fa_tag=v0.X.Y`).

## Troubleshooting

**`ansible -m ping` fails on one host.** Check SSH key auth (`ssh -v ubuntu@<ip>`)
and that `ansible_user` matches the VM's deploy user.

**`01-bootstrap` fails at "Install k3s".** Most likely the VM can't reach
`get.k3s.io`. Confirm with `ssh ubuntu@<ip> curl -fsSL https://get.k3s.io | head -c100`.

**Any Pod stuck in `ImagePullBackOff`.** Most likely a transient Docker Hub
issue or your VMs lost outbound HTTPS. Confirm with
`ssh ubuntu@<vm> docker pull docker.io/kazem26/federation-autoscaler-broker:v0.1`
(or whichever component / tag the playbook is using). If that fails, the
maintainer may have changed registry visibility — open an issue.

**Agents heartbeat with `403 Forbidden`.** The agent cert's CN doesn't match
the inventory's `cluster_id`. Re-run `02-deploy.yaml` — the `fa_identity_stamp`
role patches this. If still broken, `kubectl --kubeconfig … -n federation-autoscaler-system
get certificate agent-client -o jsonpath='{.spec.commonName}'` and confirm
it matches inventory.

**Broker shows `tls: handshake error: bad certificate` from agents.** The
CA-Secret mirror failed for that cluster. Re-run `02-deploy.yaml` — the
`fa_pki_mirror` role is idempotent.

**Broker pod CrashLoopBackOff with `unable to load server keypair`.**
cert-manager hadn't issued the broker's server certificate yet. Usually
self-heals within ~30 s; confirm with `kubectl --kubeconfig ~/.kube/central.yaml
-n federation-autoscaler-system get certificate broker-server` (READY
should be `True`) and check the broker Pod's next restart comes up clean.

**Reservation stuck at `GeneratingKubeconfig`.** The chosen provider's
agent isn't polling the broker. Check
`kubectl --kubeconfig ~/.kube/provider-1.yaml -n federation-autoscaler-system
logs deploy/agent --tail=100`; the common cause is `brokerUrl` in the
`agent-config` ConfigMap not pointing at the central VM's NodePort
(`https://<central-ip>:30443`). Re-run `02-deploy.yaml` — it re-patches the
ConfigMap from the inventory.

**Reservation reaches `Peering` then flips to `Failed`.** `liqoctl peer`
failed on the consumer agent. Tail
`kubectl --kubeconfig ~/.kube/consumer-1.yaml -n federation-autoscaler-system
logs deploy/agent --tail=200` for the underlying stderr.
