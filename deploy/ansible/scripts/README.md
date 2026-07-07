# One-command demo — `demo-up.sh`

Stand up the **entire** federation-autoscaler demo from a single control host
with one command. `demo-up.sh` wraps the Ansible install: it installs any missing
control-host tooling, generates `deploy/ansible/inventory.yaml` from the IPs you
pass, distributes your SSH key to every VM, and runs all four playbooks in order
(`04-teardown → 01-bootstrap → 02-deploy → 03-verify`).

This is the fastest path to a working demo. If you'd rather drive the same install
**step by step** — to see each stage, tune variables, or fold it into your own
automation — use the playbooks directly: see [../README.md](../README.md). For a
production-style deploy where each cluster is administered separately, use the
per-cluster scripts under [../../standalone/](../../standalone/README.md) instead.

---

## Prerequisites

- **VMs:** 1 central + 1 consumer + 2 providers, Ubuntu 22.04/24.04 on the same
  private network. Add **one more** with `--mocks <ip>` for the eco/latency
  placement strategies (omit it for the price-only demo). Per-VM sizing and the
  ports to open between them: see
  [../README.md](../README.md) → *Hardware requirements* / *Network requirements*.
- **Control host:** an Ubuntu machine (your laptop or a jump box) with `sudo` and
  outbound HTTPS. Everything else — `ansible-core`, `kubectl`, `kustomize`,
  `liqoctl`, `helm` — is installed for you if missing (pinned versions).
- **Access:** SSH to each VM as a user with **passwordless sudo**. The script
  pushes your SSH public key with `ssh-copy-id` (generating an `id_ed25519`
  keypair first if you have none), and verifies passwordless sudo on every host
  before running anything — printing the exact command to fix it if it's missing.

---

## Run it

```bash
curl -fsSL https://raw.githubusercontent.com/netgroup-polito/federation-autoscaler/main/deploy/ansible/scripts/demo-up.sh -o demo-up.sh
chmod +x demo-up.sh
./demo-up.sh --central <ip> --consumers <ip> --providers <ip>,<ip>
```

Example — the verified 1 + 1 + 2 topology, with the mock cluster and a pinned
image tag:

```bash
./demo-up.sh --central 172.23.6.90 --consumers 172.23.6.91 \
             --providers 172.23.6.92,172.23.6.93 --mocks 172.23.6.94 --tag v0.2.7
```

The first run takes ~15–25 min (k3s install + image pulls). Re-running is safe:
`demo-up.sh` starts with `04-teardown`, so every run gives a clean slate.

---

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--central <ip>` | — (required) | central / broker VM |
| `--consumers <ip[,ip...]>` | — (required) | consumer VM(s) |
| `--providers <ip[,ip...]>` | — (required) | provider VM(s) |
| `--mocks <ip>` | none | one extra VM hosting mock-eco/mock-geo for the eco & latency strategies; omit for the price-only demo |
| `--tag <tag>` | repo default (`fa_tag`) | federation-autoscaler image tag (broker / agent / grpc-server / mocks) |
| `--user <ssh-user>` | `ubuntu` | SSH login user on the VMs |
| `--ssh-key <path>` | auto-detect / generate | SSH private key to use and push |
| `--skip-ssh-copy-id` | off | don't run `ssh-copy-id` (keys already in place) |
| `--repo <git-url>` | project repo | clone source |
| `--dir <path>` | `~/federation-autoscaler` | clone / checkout location |

Cluster IDs and non-overlapping pod/service CIDRs are assigned automatically from
the IPs (1 central, `consumer-N`, `provider-N`, optional `mocks`).

---

## After it finishes

The script prints the dashboard URLs and how to drive the demo, in the order
**broker → providers → consumers**. Per-cluster kubeconfigs land at
`~/.kube/<cluster_id>.yaml` on the control host.

To drive the placement scenarios (Price / Eco / Latency) and scale up/down from
the browser, follow the **Dashboards** section of the [main README](../../../README.md),
or the full scenario walkthrough (Scenario A / B, live repricing) in
[../README.md](../README.md) → *Run the demo*.

---

## Tear down

`demo-up.sh` runs `04-teardown` at the start of every run, so re-running it wipes
and rebuilds. To tear down **without** redeploying, run the teardown playbook
directly (destructive — it uninstalls k3s on every VM):

```bash
cd ~/federation-autoscaler/deploy/ansible
ansible-playbook -i inventory.yaml playbooks/04-teardown.yaml
```
