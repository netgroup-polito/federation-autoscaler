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
| `provider-1` | 4    | 8 GB  | 30 GB    | Liqo gateway server + the offloaded shadow workload. 4/8 advertises 2× "standard" chunks (2 vCPU / 4 GB each), which is what the demo workload consumes. |
| `provider-2` | 4    | 8 GB  | 30 GB    | Same — second provider on standby so the broker has a placement choice.                                 |

**OS**: Ubuntu 22.04 or 24.04, x86_64.

If you'd rather have homogeneous boxes, 4 vCPU / 8 GB / 30 GB on **all four**
is fine and removes the asymmetric-sizing worry.

## Network requirements

All four VMs need to be on the same private network with reachability to each
other (a single VLAN, a flat cloud VPC, or four VMs on the same hypervisor
all work). Open the following ports between them:

| Port range          | Protocol | Used by                              |
| ------------------- | -------- | ------------------------------------ |
| 22                  | TCP      | SSH from your laptop                  |
| 6443                | TCP      | k3s apiserver (used by `kubectl`)     |
| 30443               | TCP      | Broker NodePort (agents → broker)     |
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
```

Takes ~5–8 min. Brings up the broker on central, agents on consumers and
providers, gRPC server on consumer, Cluster Autoscaler on consumer. Mirrors
the broker's PKI to every agent cluster. Stamps each agent's TLS cert with
its inventory cluster_id.

## Step 4 — Verify

```bash
ansible-playbook -i inventory.yaml playbooks/03-verify.yaml
```

Prints an OK banner per check. If a Deployment isn't Available it tells
you which one. After this returns green, your federation is ready to demo.

## Step 5 — Run the demo

Open four terminals. In each:

```bash
# Terminal "central"
alias k='kubectl --kubeconfig ~/.kube/central.yaml'
# Terminal "consumer-1"
alias k='kubectl --kubeconfig ~/.kube/consumer-1.yaml'
# Terminal "provider-1"
alias k='kubectl --kubeconfig ~/.kube/provider-1.yaml'
# Terminal "provider-2"
alias k='kubectl --kubeconfig ~/.kube/provider-2.yaml'
```

The demo narration (workload submit + watches + scale-down) is identical
to the Kind walkthrough in `docs/getting-started.md §6`. Two real-cluster
differences:

- Pods will reach `Running` (Liqo offloads shadow Pods through the
  WireGuard tunnel successfully on real k3s — unlike Kind-on-shared-network).
- Scale-down is **fully automatic**: delete the workload, wait
  `scale_down_unneeded_time` (1 min by default — see
  `group_vars/consumers.yaml`), and Cluster Autoscaler fires
  `NodeGroupDeleteNodes` on its own. No manual `kubectl delete reservation`
  needed.

## Teardown

```bash
ansible-playbook -i inventory.yaml playbooks/04-teardown.yaml
```

Removes federation-autoscaler resources first (so finalizers run cleanly),
then uninstalls Liqo, then leaves k3s in place — uninstalling k3s itself
is destructive; run `/usr/local/bin/k3s-uninstall.sh` on each VM manually
if you actually want the boxes wiped.

## Where the variables live

- `group_vars/all.yaml` — image registry/tag, software versions, broker
  exposure (NodePort + central IP).
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

Then bump `fa_tag` in `group_vars/all.yaml` so the playbook pulls the new
version on the next `02-deploy.yaml` run.

## Troubleshooting

**`ansible -m ping` fails on one host.** Check SSH key auth (`ssh -v ubuntu@<ip>`)
and that `ansible_user` matches the VM's deploy user.

**`01-bootstrap` fails at "Install k3s".** Most likely the VM can't reach
`get.k3s.io`. Confirm with `ssh ubuntu@<ip> curl -fsSL https://get.k3s.io | head -c100`.

**Any Pod stuck in `ImagePullBackOff`.** Most likely a transient Docker Hub
issue or your VMs lost outbound HTTPS. Confirm with
`ssh ubuntu@<vm> docker pull docker.io/kazem26/federation-autoscaler-broker:v0.1.0`
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
