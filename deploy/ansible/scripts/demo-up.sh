#!/usr/bin/env bash
#
# demo-up.sh — one-command federation-autoscaler demo bring-up.
#
# Does everything needed to stand up the demo from a bare Ubuntu control
# host:
#   1. Installs any missing control-host tools (ansible-core, kubectl,
#      kustomize, liqoctl) — pinned to the versions the project expects.
#   2. Clones (or refreshes) the repo, then generates deploy/ansible/
#      inventory.yaml from the IPs you pass in (1 central + N consumers +
#      M providers, with auto-assigned cluster IDs and non-overlapping
#      pod/service CIDRs).
#   3. Pushes your SSH public key to every VM with ssh-copy-id (generating
#      ~/.ssh/id_ed25519 first if you have no key yet). You may be prompted
#      for each VM's login password the first time; it's a no-op per host
#      once the key is installed. Disable with --skip-ssh-copy-id.
#   4. Pings every host with Ansible and verifies passwordless sudo; only
#      if all pass does it run the playbooks in order: 04-teardown ->
#      01-bootstrap -> 02-deploy -> 03-verify.
#   5. Prints an idle-baseline status snapshot of every cluster (providers
#      advertising, the consumer's local capacity + components + zero
#      borrowed nodes, and the incoming workload's demand) so the whole
#      "before scale-up" state is visible at a glance.
#
# Usage:
#   demo-up.sh --central <ip> --consumers <ip[,ip...]> --providers <ip[,ip...]>
#              [--user <ssh-user>] [--ssh-key <path>] [--skip-ssh-copy-id]
#              [--repo <git-url>] [--dir <clone-dir>]
#
# Example (the verified 1+1+2 demo topology):
#   demo-up.sh --central 172.23.6.90 --consumers 172.23.6.91 \
#              --providers 172.23.6.92,172.23.6.93
#
# Assumes: Ubuntu 22.04/24.04 control host with sudo + outbound HTTPS. SSH
# key auth is set up for you via ssh-copy-id (step 3); passwordless sudo on
# the VMs is checked (step 4) and, if missing, the script prints the exact
# command to fix it.

set -euo pipefail

# -----------------------------------------------------------------------------
# Defaults (override via flags or env)
# -----------------------------------------------------------------------------
REPO_URL="${REPO_URL:-https://github.com/netgroup-polito/federation-autoscaler}"
CLONE_DIR="${CLONE_DIR:-$HOME/federation-autoscaler}"
SSH_USER="${SSH_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-}"
SKIP_SSH_COPY=""

CENTRAL=""
CONSUMERS=""
PROVIDERS=""

# Tool versions — keep in lock-step with deploy/ansible/README.md.
KUBECTL_VERSION="v1.32.5"
LIQOCTL_VERSION="v1.1.2"

# -----------------------------------------------------------------------------
# Helpers
# -----------------------------------------------------------------------------
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✔\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m  ! \033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# Print the header comment block (from line 3 to the first non-comment line).
usage() { awk 'NR>2{ if (/^#/) { sub(/^# ?/,""); print } else { exit } }' "$0"; exit "${1:-0}"; }

# -----------------------------------------------------------------------------
# Parse arguments
# -----------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --central)   CENTRAL="$2"; shift 2 ;;
    --consumers) CONSUMERS="$2"; shift 2 ;;
    --providers) PROVIDERS="$2"; shift 2 ;;
    --user)      SSH_USER="$2"; shift 2 ;;
    --ssh-key)   SSH_KEY="$2"; shift 2 ;;
    --skip-ssh-copy-id) SKIP_SSH_COPY=1; shift ;;
    --repo)      REPO_URL="$2"; shift 2 ;;
    --dir)       CLONE_DIR="$2"; shift 2 ;;
    -h|--help)   usage 0 ;;
    *)           die "unknown argument: $1 (see --help)" ;;
  esac
done

[[ -n "$CENTRAL"   ]] || die "--central <ip> is required (see --help)"
[[ -n "$CONSUMERS" ]] || die "--consumers <ip[,ip...]> is required"
[[ -n "$PROVIDERS" ]] || die "--providers <ip[,ip...]> is required"

IFS=',' read -ra CONSUMER_IPS <<< "$CONSUMERS"
IFS=',' read -ra PROVIDER_IPS <<< "$PROVIDERS"
# CIDR third octet stays <= 255: consumers use 50+i / 150+i, providers
# 100+j / 200+j, so guard the counts before we generate colliding ranges.
(( ${#CONSUMER_IPS[@]} >= 1 && ${#CONSUMER_IPS[@]} <= 49 )) || die "1..49 consumers supported, got ${#CONSUMER_IPS[@]}"
(( ${#PROVIDER_IPS[@]} >= 1 && ${#PROVIDER_IPS[@]} <= 55 )) || die "1..55 providers supported, got ${#PROVIDER_IPS[@]}"

# -----------------------------------------------------------------------------
# 1. Control-host tools (install only what's missing)
# -----------------------------------------------------------------------------
log "Checking control-host tools"

ensure_apt_updated() {
  if [[ -z "${_APT_UPDATED:-}" ]]; then
    sudo apt-get update -y >/dev/null
    _APT_UPDATED=1
  fi
}

if ! command -v ansible-playbook >/dev/null 2>&1; then
  warn "ansible not found — installing ansible-core"
  ensure_apt_updated
  sudo apt-get install -y ansible-core >/dev/null
fi
ok "ansible: $(ansible --version | head -1)"

if ! command -v kubectl >/dev/null 2>&1; then
  warn "kubectl not found — installing ${KUBECTL_VERSION}"
  tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/kubectl" "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
  sudo install -m0755 "$tmp/kubectl" /usr/local/bin/kubectl
  rm -rf "$tmp"
fi
ok "kubectl: $(kubectl version --client -o yaml 2>/dev/null | awk '/gitVersion/{print $2; exit}')"

if ! command -v kustomize >/dev/null 2>&1; then
  warn "kustomize not found — installing latest v5"
  tmp="$(mktemp -d)"; ( cd "$tmp" && curl -s "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh" | bash >/dev/null )
  sudo install -m0755 "$tmp/kustomize" /usr/local/bin/kustomize
  rm -rf "$tmp"
fi
ok "kustomize: $(kustomize version 2>/dev/null)"

if ! command -v liqoctl >/dev/null 2>&1; then
  warn "liqoctl not found — installing ${LIQOCTL_VERSION}"
  tmp="$(mktemp -d)"
  curl -fsSL "https://github.com/liqotech/liqo/releases/download/${LIQOCTL_VERSION}/liqoctl-linux-amd64.tar.gz" | tar -xz -C "$tmp" liqoctl
  sudo install -m0755 "$tmp/liqoctl" /usr/local/bin/liqoctl
  rm -rf "$tmp"
fi
ok "liqoctl: $(liqoctl version --client 2>/dev/null | awk '/Client version/{print $3; exit}')"

# -----------------------------------------------------------------------------
# 2. Clone / refresh the repo, then generate the inventory
# -----------------------------------------------------------------------------
if [[ -d "$CLONE_DIR/.git" ]]; then
  log "Repo already present at $CLONE_DIR — pulling latest"
  git -C "$CLONE_DIR" pull --ff-only || warn "git pull failed (continuing with the checkout on disk)"
else
  log "Cloning $REPO_URL -> $CLONE_DIR"
  git clone "$REPO_URL" "$CLONE_DIR"
fi

ANSIBLE_DIR="$CLONE_DIR/deploy/ansible"
[[ -d "$ANSIBLE_DIR" ]] || die "$ANSIBLE_DIR not found — wrong repo?"
cd "$ANSIBLE_DIR"

INVENTORY="$ANSIBLE_DIR/inventory.yaml"
if [[ -f "$INVENTORY" ]]; then
  cp "$INVENTORY" "$INVENTORY.bak.$(date +%s 2>/dev/null || echo prev)" 2>/dev/null || true
  warn "existing inventory.yaml backed up"
fi

log "Generating inventory.yaml (1 central, ${#CONSUMER_IPS[@]} consumer(s), ${#PROVIDER_IPS[@]} provider(s))"
{
  echo "# Generated by demo-up.sh — edit by hand only for one-off tweaks."
  echo "all:"
  echo "  vars:"
  echo "    ansible_user: ${SSH_USER}"
  [[ -n "$SSH_KEY" ]] && echo "    ansible_ssh_private_key_file: ${SSH_KEY}"
  echo "  children:"
  echo "    central:"
  echo "      hosts:"
  echo "        fa-central:"
  echo "          ansible_host: ${CENTRAL}"
  echo "          cluster_id: central"
  echo "          pod_cidr: 10.40.0.0/16"
  echo "          service_cidr: 10.140.0.0/16"
  echo "    consumers:"
  echo "      hosts:"
  i=0
  for ip in "${CONSUMER_IPS[@]}"; do
    i=$((i + 1))
    echo "        fa-consumer-${i}:"
    echo "          ansible_host: ${ip}"
    echo "          cluster_id: consumer-${i}"
    echo "          pod_cidr: 10.$((50 + i)).0.0/16"
    echo "          service_cidr: 10.$((150 + i)).0.0/16"
  done
  echo "    providers:"
  echo "      hosts:"
  j=0
  for ip in "${PROVIDER_IPS[@]}"; do
    j=$((j + 1))
    echo "        fa-provider-${j}:"
    echo "          ansible_host: ${ip}"
    echo "          cluster_id: provider-${j}"
    echo "          pod_cidr: 10.$((100 + j)).0.0/16"
    echo "          service_cidr: 10.$((200 + j)).0.0/16"
  done
} > "$INVENTORY"
ok "wrote $INVENTORY"

# -----------------------------------------------------------------------------
# 3. Distribute the SSH public key to every host (ssh-copy-id)
# -----------------------------------------------------------------------------
if [[ -n "$SKIP_SSH_COPY" ]]; then
  warn "skipping ssh-copy-id (--skip-ssh-copy-id)"
else
  log "Distributing SSH key to all hosts (you may be prompted for each VM's password the first time)"
  command -v ssh-copy-id >/dev/null 2>&1 || { ensure_apt_updated; sudo apt-get install -y openssh-client >/dev/null; }

  # Resolve the public key to push: explicit --ssh-key wins, else a default
  # identity, else generate an ed25519 keypair so a bare host still works.
  if [[ -n "$SSH_KEY" ]]; then
    PUBKEY="${SSH_KEY}.pub"
    [[ -f "$PUBKEY" ]] || die "public key $PUBKEY not found (expected alongside --ssh-key $SSH_KEY)"
  elif [[ -f "$HOME/.ssh/id_ed25519.pub" ]]; then
    PUBKEY="$HOME/.ssh/id_ed25519.pub"
  elif [[ -f "$HOME/.ssh/id_rsa.pub" ]]; then
    PUBKEY="$HOME/.ssh/id_rsa.pub"
  else
    warn "no SSH key found — generating $HOME/.ssh/id_ed25519"
    mkdir -p "$HOME/.ssh"; chmod 700 "$HOME/.ssh"
    ssh-keygen -t ed25519 -N "" -f "$HOME/.ssh/id_ed25519" -q
    PUBKEY="$HOME/.ssh/id_ed25519.pub"
  fi
  ok "using public key $PUBKEY"

  for ip in "$CENTRAL" "${CONSUMER_IPS[@]}" "${PROVIDER_IPS[@]}"; do
    # accept-new auto-trusts first-seen host keys so we don't hang on the
    # interactive yes/no prompt; idempotent once the key is installed.
    if ssh-copy-id -i "$PUBKEY" -o StrictHostKeyChecking=accept-new "${SSH_USER}@${ip}" >/dev/null 2>&1; then
      ok "key present on ${ip}"
    else
      warn "ssh-copy-id could not reach ${ip} as ${SSH_USER} (the ping step below will report it)"
    fi
  done
fi

# -----------------------------------------------------------------------------
# 4. Reachability check, then run the playbooks in order
# -----------------------------------------------------------------------------
log "Pinging all hosts (ansible -m ping)"
if ! ansible -i "$INVENTORY" all -m ping; then
  die "one or more hosts are unreachable. Check the IPs, that SSH key auth works as '${SSH_USER}', and that passwordless sudo is configured. Fix and re-run."
fi
ok "all hosts reachable"

log "Verifying passwordless sudo on all hosts"
SUDO_FAIL=()
for ip in "$CENTRAL" "${CONSUMER_IPS[@]}" "${PROVIDER_IPS[@]}"; do
  if ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new "${SSH_USER}@${ip}" 'sudo -n true' >/dev/null 2>&1; then
    ok "passwordless sudo on ${ip}"
  else
    SUDO_FAIL+=("$ip")
    warn "passwordless sudo NOT available on ${ip}"
  fi
done
if (( ${#SUDO_FAIL[@]} > 0 )); then
  cat >&2 <<EOF

The bootstrap playbooks install k3s + Liqo with 'become' (sudo), so every
VM needs passwordless sudo for '${SSH_USER}'. Configure it on each failing
host (run as a user that can sudo), then re-run this script:

  echo '${SSH_USER} ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/${SSH_USER}-nopasswd
  sudo chmod 440 /etc/sudoers.d/${SSH_USER}-nopasswd
EOF
  die "passwordless sudo missing on: ${SUDO_FAIL[*]}"
fi
ok "passwordless sudo confirmed on all hosts"

run_play() {
  local pb="$1"
  log "Running playbooks/${pb}"
  ansible-playbook -i "$INVENTORY" "playbooks/${pb}" \
    || die "playbooks/${pb} failed — see the output above"
  ok "${pb} complete"
}

run_play 04-teardown.yaml
run_play 01-bootstrap.yaml
run_play 02-deploy.yaml
run_play 03-verify.yaml

# -----------------------------------------------------------------------------
# 5. Idle-baseline status snapshot (the "before scale-up" picture)
# -----------------------------------------------------------------------------
NS="federation-autoscaler-system"
kc() { echo "$HOME/.kube/${1}.yaml"; }

# banner <text> — a spaced, ruled cluster section header. Left-anchored title
# + a fixed-width bottom rule, so multibyte chars in the title never skew it.
banner() {
  printf '\n\033[1;36m┏━ \033[0m\033[1m%s\033[0m\n' "$1"
  printf   '\033[1;36m┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\033[0m\n'
}

# check <cluster> <title> <expect> <kubectl args...> — print the title, the exact
# command that produced the result, what to expect, then the indented result
# (or "(none)" when empty). set -e-safe via || true.
check() {
  local cluster="$1" title="$2" expect="$3"; shift 3
  printf '\n  \033[1m▸ %s\033[0m\n' "$title"
  printf '    \033[2m$ kubectl --kubeconfig ~/.kube/%s.yaml %s\033[0m\n' "$cluster" "$*"
  printf '    \033[33mexpect:\033[0m %s\n' "$expect"
  local out; out="$(kubectl --kubeconfig "$(kc "$cluster")" "$@" 2>/dev/null)" || true
  if [[ -n "$out" ]]; then sed 's/^/        /' <<<"$out"; else echo "        (none)"; fi
}

# lcheck — same as check() but runs liqoctl instead of kubectl (e.g. liqoctl info).
lcheck() {
  local cluster="$1" title="$2" expect="$3"; shift 3
  printf '\n  \033[1m▸ %s\033[0m\n' "$title"
  printf '    \033[2m$ liqoctl --kubeconfig ~/.kube/%s.yaml %s\033[0m\n' "$cluster" "$*"
  printf '    \033[33mexpect:\033[0m %s\n' "$expect"
  local out; out="$(liqoctl --kubeconfig "$(kc "$cluster")" "$@" 2>/dev/null)" || true
  if [[ -n "$out" ]]; then sed 's/^/        /' <<<"$out"; else echo "        (none)"; fi
}

echo
log "Environment status — idle baseline (before scale-up)"

banner "central (broker)  —  ${CENTRAL}"
check central "Provider advertisements (chunk budget)" \
  "one row per provider; READY=true, RESERVED empty/0, AVAILABLE=TOTAL — capacity offered, none lent yet" \
  -n "$NS" get clusteradvertisements
check central "Reservations (the phase machine)" \
  "none — no scale-up has happened, so no chunk is reserved" \
  -n "$NS" get reservations --no-headers

i=0
for ip in "${CONSUMER_IPS[@]}"; do
  i=$((i + 1)); c="consumer-${i}"
  banner "${c} (cluster-autoscaler)  —  ${ip}"
  lcheck "$c" "Liqo peering status" \
    "Liqo healthy; NO active peerings yet — a peering to a provider shows up here once scale-up borrows a chunk" \
    info
  check "$c" "Local node allocatable" \
    "the workload runs HERE first; only the Pods that don't fit in the free CPU/MEM spill to the federation" \
    get nodes -o custom-columns='NODE:.metadata.name,CPU:.status.allocatable.cpu,MEM:.status.allocatable.memory,PODS:.status.allocatable.pods'
  check "$c" "Federation components" \
    "agent, grpc-server, cluster-autoscaler all READY = WANT (1)" \
    -n "$NS" get deploy agent grpc-server cluster-autoscaler -o custom-columns='DEPLOY:.metadata.name,READY:.status.readyReplicas,WANT:.spec.replicas'
  check "$c" "Borrowed virtual nodes" \
    "none yet — nothing is borrowed until we scale up" \
    get nodes -l liqo.io/type=virtual-node --no-headers
  check "$c" "NamespaceOffloading" \
    "one named 'offloading' (strategy LocalAndRemote) — Pods run locally first, overflow offloads to providers" \
    get namespaceoffloadings -A
done

j=0
for ip in "${PROVIDER_IPS[@]}"; do
  j=$((j + 1)); p="provider-${j}"
  banner "${p}  —  ${ip}"
  check "$p" "Provider agent" \
    "READY = WANT (1) — it advertises this cluster's capacity to the broker every 30s" \
    -n "$NS" get deploy agent -o custom-columns='DEPLOY:.metadata.name,READY:.status.readyReplicas,WANT:.spec.replicas'
  check "$p" "Allocatable headroom to lend" \
    "spare CPU/MEM the broker carves into chunks (2 CPU / 4Gi each) and lends on demand" \
    get nodes -o custom-columns='NODE:.metadata.name,CPU:.status.allocatable.cpu,MEM:.status.allocatable.memory,PODS:.status.allocatable.pods'
done

banner "incoming workload  —  samples/burst-workload.yaml"
printf '\n  \033[1m▸ Workload demand\033[0m\n'
printf '    \033[2m$ awk (sum replicas x per-Pod cpu/memory requests)\033[0m\n'
printf "    \033[33mexpect:\033[0m exceeds the consumer's free capacity, so the overflow Pods stay Pending\n"
printf '            -> CA borrows a chunk from EACH provider (consumer + both providers all used)\n'
awk '
  /replicas:/ && reps=="" { for (k=1;k<=NF;k++) if ($k ~ /^[0-9]+$/) reps=$k }
  /cpu:/      && cpu==""  { v=$2; gsub(/"/,"",v); cpu=v }
  /memory:/   && mem==""  { v=$2; gsub(/"/,"",v); mem=v }
  END {
    if (reps=="" || cpu=="" || mem=="") { print "        (could not parse workload)"; exit }
    if (cpu ~ /m$/) { c=cpu; sub(/m$/,"",c); cmv=c+0 } else { cmv=(cpu+0)*1000 }
    if (mem ~ /Gi$/) { m=mem; sub(/Gi$/,"",m); mmv=(m+0)*1024 } else { m=mem; sub(/Mi$/,"",m); mmv=m+0 }
    printf "        %s replicas x %s CPU / %s  =>  ~%.1f CPU / %.1fGi total demand\n", reps, cpu, mem, cmv*reps/1000, mmv*reps/1024
  }
' "$ANSIBLE_DIR/samples/burst-workload.yaml" 2>/dev/null || echo "        (workload sample not found)"

# -----------------------------------------------------------------------------
# Done
# -----------------------------------------------------------------------------
echo
ok "Federation is up. Per-cluster kubeconfigs are under ~/.kube/<cluster_id>.yaml"
cat <<EOF

Next steps — drive the scale-up / scale-down demo:

  1) In a SECOND terminal, open the live dashboard and leave it running. It
     watches all four clusters side by side — reservations, virtual nodes, and
     the consumer's Pods going Pending -> Running (local vs borrowed):
       $ANSIBLE_DIR/scripts/demo-watch.sh

  2) Back HERE, scale UP — apply the workload on the consumer:
       kubectl --kubeconfig ~/.kube/consumer-1.yaml apply -f "$ANSIBLE_DIR/samples/burst-workload.yaml"
     On the dashboard: ~3 Pods Run locally, the overflow pends, then lands on a
     borrowed chunk from each provider (consumer + both providers all in use).

  3) Scale DOWN — delete the workload; Cluster Autoscaler reclaims the borrowed
     nodes (give it ~1-2 min):
       kubectl --kubeconfig ~/.kube/consumer-1.yaml delete -f "$ANSIBLE_DIR/samples/burst-workload.yaml"

  (The dashboard's consumer pane already shows the Pods live, so a separate
   'kubectl get pods -o wide -w' isn't needed — it's the same view.)
EOF
