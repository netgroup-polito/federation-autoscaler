#!/usr/bin/env bash
#
# demo-up.sh — one-command federation-autoscaler demo bring-up.
#
# Does everything needed to stand up the demo from a bare Ubuntu control
# host:
#   1. Installs any missing control-host tools (ansible-core, kubectl,
#      kustomize, liqoctl, helm) — pinned to the versions the project expects.
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
#   5. Prints how to reach the dashboards (broker, Liqo, and the per-cluster
#      provider/consumer config consoles) and how to drive the demo, in the
#      order broker -> providers -> consumers.
#
# Usage:
#   demo-up.sh --central <ip> --consumers <ip[,ip...]> --providers <ip[,ip...]>
#              [--mocks <ip>] [--tag <image-tag>] [--user <ssh-user>]
#              [--ssh-key <path>] [--skip-ssh-copy-id] [--repo <git-url>]
#              [--dir <clone-dir>]
#
#   --tag overrides the federation-autoscaler image tag the deploy pulls
#   (broker/agent/grpc-server/mock-eco/mock-geo), e.g. --tag v0.2.7. Omit it to
#   use the repo default (group_vars/all.yaml: fa_tag).
#
#   --mocks <ip> adds ONE extra single-node VM as the dedicated mock cluster
#   (option A) hosting mock-eco + mock-geo for the eco/latency placement
#   strategies. Its address is auto-wired into every agent. Omit it to run the
#   price-only demo (eco/latency stay inert). Each cluster's location is
#   auto-discovered from its node IP (mock-geo maps IP → region + coords); set a
#   per-host fa_advertised_ip to steer a cluster to a specific city.
#
# Example (the verified 1+1+2 demo topology):
#   demo-up.sh --central 172.23.6.90 --consumers 172.23.6.91 \
#              --providers 172.23.6.92,172.23.6.93 --tag v0.2.7
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
MOCKS=""    # optional single mock-cluster IP (eco/latency strategies, option A)
FA_TAG=""   # empty ⇒ use the repo default (group_vars/all.yaml: fa_tag)

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
    --mocks)     MOCKS="$2"; shift 2 ;;
    --tag)       FA_TAG="$2"; shift 2 ;;
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

# Every host the reachability / key-distribution steps must touch. The optional
# mock cluster (--mocks) is one extra single-node VM that hosts mock-eco/mock-geo.
ALL_IPS=("$CENTRAL" "${CONSUMER_IPS[@]}" "${PROVIDER_IPS[@]}")
[[ -n "$MOCKS" ]] && ALL_IPS+=("$MOCKS")

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

# helm is used by 02-deploy to install the Liqo dashboard on the consumer.
if ! command -v helm >/dev/null 2>&1; then
  warn "helm not found — installing latest Helm (get-helm-4)"
  tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/get-helm.sh" https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4
  chmod 700 "$tmp/get-helm.sh"
  ( cd "$tmp" && ./get-helm.sh >/dev/null )
  rm -rf "$tmp"
fi
ok "helm: $(helm version --short 2>/dev/null || echo installed)"

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

log "Generating inventory.yaml (1 central, ${#CONSUMER_IPS[@]} consumer(s), ${#PROVIDER_IPS[@]} provider(s)$([[ -n "$MOCKS" ]] && echo ", 1 mocks"))"
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
  # Optional dedicated mock cluster (--mocks): hosts mock-eco/mock-geo for the
  # eco/latency strategies. group_vars resolves its address into --mock-*-url
  # for every agent; the 02-deploy fa_mocks play targets this group.
  if [[ -n "$MOCKS" ]]; then
    echo "    mocks:"
    echo "      hosts:"
    echo "        fa-mocks:"
    echo "          ansible_host: ${MOCKS}"
    echo "          cluster_id: mocks"
    echo "          pod_cidr: 10.80.0.0/16"
    echo "          service_cidr: 10.180.0.0/16"
  fi
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

  for ip in "${ALL_IPS[@]}"; do
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
for ip in "${ALL_IPS[@]}"; do
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

# Extra Ansible vars common to every play. --tag overrides fa_tag (the image
# tag for broker/agent/grpc-server); -e has the highest precedence, so it wins
# over group_vars/all.yaml without editing any file.
EXTRA_VARS=()
if [[ -n "$FA_TAG" ]]; then
  EXTRA_VARS+=(-e "fa_tag=${FA_TAG}")
  log "Using federation-autoscaler image tag: ${FA_TAG}"
fi

run_play() {
  local pb="$1"
  log "Running playbooks/${pb}"
  ansible-playbook -i "$INVENTORY" "${EXTRA_VARS[@]}" "playbooks/${pb}" \
    || die "playbooks/${pb} failed — see the output above"
  ok "${pb} complete"
}

run_play 04-teardown.yaml
run_play 01-bootstrap.yaml
run_play 02-deploy.yaml
run_play 03-verify.yaml

# -----------------------------------------------------------------------------
# Done
# -----------------------------------------------------------------------------
echo
ok "Federation is up. Per-cluster kubeconfigs are under ~/.kube/<cluster_id>.yaml"
cat <<EOF

Next steps — drive the demo from the browser in this order
(broker -> providers -> consumers, matching the demo flow):

──────────────────────────────────────────────────────────────────────────
 1) BROKER  (central) — watch the federation decide
──────────────────────────────────────────────────────────────────────────
  Read-only dashboard:  http://${CENTRAL}:30444/
  Shows every provider advertisement (cost · carbon · auto-discovered location),
  live reservations, the instruction phase machine, and chunk capacity. Keep it
  open — it is where you SEE which provider the broker picks for each policy.

──────────────────────────────────────────────────────────────────────────
 2) PROVIDERS — advertise what each one offers
──────────────────────────────────────────────────────────────────────────
  Config console (set unit prices, advertised CPU/RAM %, renewable flag, then
  Submit; changes reach the broker on the next advertisement ~30 s. The console
  also shows this provider's auto-discovered location read-only):
EOF
j=0
for ip in "${PROVIDER_IPS[@]}"; do
  j=$((j + 1))
  echo "    • provider-${j}:  http://${ip}:30445/"
done
if [[ -z "$MOCKS" ]]; then
  cat <<EOF
  NOTE: eco/latency need the mock cluster — re-run with --mocks <ip>. Without
  it, location/carbon are inert and only the Price policy is meaningful.
EOF
fi

cat <<EOF

──────────────────────────────────────────────────────────────────────────
 3) CONSUMERS — pick a policy and drive scale up / down
──────────────────────────────────────────────────────────────────────────
  Liqo dashboard (peerings, virtual nodes, offloaded pods):
    add this line to your machine's hosts file (e.g. /etc/hosts):
      ${CONSUMER_IPS[0]} liqo-dashboard.local
    then browse:  http://liqo-dashboard.local

  Config console (pick the placement policy — Standard / Price / Eco /
  Latency — then flip the workload switch ON to scale up / OFF to scale down;
  watch the broker dashboard react. Location is auto-discovered, shown read-only):
EOF
i=0
for ip in "${CONSUMER_IPS[@]}"; do
  i=$((i + 1))
  echo "    • consumer-${i}:  http://${ip}:30445/"
done

cat <<EOF

  CLI equivalent of the workload toggle (no console needed):
    kubectl --kubeconfig ~/.kube/consumer-1.yaml apply  -f "$ANSIBLE_DIR/samples/burst-workload.yaml"
    kubectl --kubeconfig ~/.kube/consumer-1.yaml delete -f "$ANSIBLE_DIR/samples/burst-workload.yaml"
EOF
