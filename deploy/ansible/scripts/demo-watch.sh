#!/usr/bin/env bash
#
# demo-watch.sh — a tmux dashboard for watching a federation-autoscaler
# scale-up / scale-down end to end across the four demo clusters.
#
#   ./demo-watch.sh          # open the dashboard (attaches a tmux session)
#   ./demo-watch.sh --kill   # tear the session down
#
# Layout (tiled, 2x2):
#   ┌────────────────────┬────────────────────┐
#   │ central (broker)   │ consumer-1 (CA)    │
#   ├────────────────────┼────────────────────┤
#   │ provider-1         │ provider-2         │
#   └────────────────────┴────────────────────┘
#
# Each pane re-invokes this script in `_pane` mode and refreshes on a timer.
# Kubeconfigs are read from $KUBECONFIG_DIR/<cluster>.yaml (the location the
# Ansible bootstrap fetches them to). Override any of the env vars below.
set -euo pipefail

SESSION="${FA_DEMO_SESSION:-fa-demo}"
KUBECONFIG_DIR="${KUBECONFIG_DIR:-$HOME/.kube}"
NAMESPACE="${FA_NAMESPACE:-federation-autoscaler-system}"
WATCH_INTERVAL="${WATCH_INTERVAL:-2}"

CENTRAL="${FA_CENTRAL:-central}"
CONSUMER="${FA_CONSUMER:-consumer-1}"
PROVIDER_1="${FA_PROVIDER_1:-provider-1}"
PROVIDER_2="${FA_PROVIDER_2:-provider-2}"

# Label selector for the burst workload (deploy/ansible/samples/burst-workload.yaml).
# Liqo preserves Pod labels when it reflects them, so the same selector finds
# the Pods on the consumer and their offloaded copies on the providers.
WORKLOAD_SELECTOR="${FA_WORKLOAD_SELECTOR:-app=federation-demo}"

kubeconfig_for() { echo "${KUBECONFIG_DIR}/${1}.yaml"; }

# sec <title> <displayed-command> — section header + the command that produced
# the block below it. expect <hint> — what to watch for as the demo runs.
sec()    { printf '\n\033[1;36m▸ %s\033[0m  \033[2m$ %s\033[0m\n' "$1" "$2"; }
expect() { printf '  \033[33mexpect:\033[0m %s\n' "$*"; }

# -----------------------------------------------------------------------------
# Pane renderers — one screen-refresh worth of output for each cluster role.
# Each block prints its command + what to watch for, then the live result.
# -----------------------------------------------------------------------------

render_central() {
  sec "ClusterAdvertisements — chunk budget" "kubectl -n $NAMESPACE get clusteradvertisements"
  expect "RESERVED rises as chunks are lent, AVAILABLE falls; both revert on scale-down"
  kubectl -n "$NAMESPACE" get clusteradvertisements 2>/dev/null || echo "  (none)"

  sec "Reservations — phase machine" "kubectl -n $NAMESPACE get reservations"
  expect "Pending → … → Peered on scale-up; → Released then GC'd on scale-down"
  kubectl -n "$NAMESPACE" get reservations 2>/dev/null || echo "  (none)"

  sec "ProviderInstructions" "kubectl -n $NAMESPACE get providerinstructions"
  expect "a GenerateKubeconfig appears during peering, then is enforced + GC'd"
  kubectl -n "$NAMESPACE" get providerinstructions 2>/dev/null || echo "  (none)"

  sec "ReservationInstructions" "kubectl -n $NAMESPACE get reservationinstructions"
  expect "Peer on scale-up; Unpeer + Cleanup on scale-down"
  kubectl -n "$NAMESPACE" get reservationinstructions 2>/dev/null || echo "  (none)"
}

render_consumer() {
  sec "Virtual nodes — borrowed capacity" "kubectl get nodes -l liqo.io/type=virtual-node"
  expect "one virtual node per borrowed chunk appears as CA scales up (gone on scale-down)"
  kubectl get nodes -l liqo.io/type=virtual-node -o wide 2>/dev/null || echo "  (none yet)"

  sec "VirtualNodeState — what CA reads" "kubectl -n $NAMESPACE get virtualnodestates"
  expect "PHASE flips to Running once the borrowed v1.Node is Ready"
  kubectl -n "$NAMESPACE" get virtualnodestates 2>/dev/null || echo "  (none)"

  sec "Workload pods — local vs borrowed" "kubectl get pods -A -l $WORKLOAD_SELECTOR -o wide"
  expect "first Pods run locally (NODE=consumer); the overflow pends, then Runs on provider virtual nodes"
  kubectl get pods -A -l "$WORKLOAD_SELECTOR" -o wide 2>/dev/null || echo "  (none)"

  sec "Cluster Autoscaler — recent decisions" "kubectl -n $NAMESPACE logs deploy/cluster-autoscaler | grep -i scale | tail"
  expect "'Scale-up' when pods pend; NodeGroupDeleteNodes when the workload is gone"
  kubectl -n "$NAMESPACE" logs deploy/cluster-autoscaler --tail=200 2>/dev/null \
    | grep -iE 'scale|expander|node group|nodegroup|unregistered|reservation' | tail -6 \
    || echo "  (no CA logs yet)"
}

render_provider() {
  sec "Nodes — allocatable headroom to lend" "kubectl get nodes -o custom-columns=NODE,CPU,MEM,PODS"
  expect "spare CPU/MEM this provider lends as 2 CPU / 4Gi chunks"
  kubectl get nodes -o custom-columns='NODE:.metadata.name,CPU:.status.allocatable.cpu,MEM:.status.allocatable.memory,PODS:.status.allocatable.pods' 2>/dev/null || echo "  (none)"

  sec "Offloaded pods reflected from the consumer" "kubectl get pods -A -l $WORKLOAD_SELECTOR -o wide"
  expect "the consumer's Pods show up here Running once scale-up offloads them"
  # Liqo reflects the consumer's Pods onto this provider (in a remapped
  # namespace) while preserving labels, so the workload selector finds them.
  kubectl get pods -A -l "$WORKLOAD_SELECTOR" -o wide 2>/dev/null || echo "  (none yet)"
}

# -----------------------------------------------------------------------------
# Pane entrypoint: `demo-watch.sh _pane <role> <cluster> <kubeconfig>`.
# tmux runs one of these per pane; it loops, clearing and re-rendering.
# -----------------------------------------------------------------------------

if [[ "${1:-}" == "_pane" ]]; then
  role="$2"; cluster="$3"; kc="$4"
  export KUBECONFIG="$kc"
  trap 'exit 0' INT TERM
  while true; do
    clear
    printf '== %s :: %s ==   (every %ss)\n\n' "$role" "$cluster" "$WATCH_INTERVAL"
    case "$role" in
      central)  render_central ;;
      consumer) render_consumer ;;
      provider) render_provider "$cluster" ;;
      *)        echo "unknown pane role: $role" ;;
    esac
    sleep "$WATCH_INTERVAL"
  done
fi

# -----------------------------------------------------------------------------
# Main: build the tmux session.
# -----------------------------------------------------------------------------

command -v tmux >/dev/null 2>&1    || { echo "demo-watch.sh: tmux is required" >&2; exit 1; }
command -v kubectl >/dev/null 2>&1 || { echo "demo-watch.sh: kubectl is required" >&2; exit 1; }

if [[ "${1:-}" == "--kill" ]]; then
  tmux kill-session -t "$SESSION" 2>/dev/null && echo "killed session $SESSION" || echo "no session $SESSION"
  exit 0
fi

for c in "$CENTRAL" "$CONSUMER" "$PROVIDER_1" "$PROVIDER_2"; do
  kc="$(kubeconfig_for "$c")"
  [[ -f "$kc" ]] || { echo "demo-watch.sh: missing kubeconfig $kc (set KUBECONFIG_DIR or FA_* vars)" >&2; exit 1; }
done

SELF="$(readlink -f "$0")"
pane_cmd() { echo "exec bash '$SELF' _pane '$1' '$2' '$(kubeconfig_for "$2")'"; }

tmux kill-session -t "$SESSION" 2>/dev/null || true

# -P -F prints the initial pane's id directly, avoiding a `list-panes | head`
# pipe that would trip `set -o pipefail` on SIGPIPE.
p_central=$(tmux new-session -d -s "$SESSION" -n demo -P -F '#{pane_id}')
tmux send-keys -t "$p_central" "$(pane_cmd central "$CENTRAL")" C-m

p_consumer=$(tmux split-window -h -t "$p_central" -P -F '#{pane_id}')
tmux send-keys -t "$p_consumer" "$(pane_cmd consumer "$CONSUMER")" C-m

p_prov1=$(tmux split-window -v -t "$p_central" -P -F '#{pane_id}')
tmux send-keys -t "$p_prov1" "$(pane_cmd provider "$PROVIDER_1")" C-m

p_prov2=$(tmux split-window -v -t "$p_consumer" -P -F '#{pane_id}')
tmux send-keys -t "$p_prov2" "$(pane_cmd provider "$PROVIDER_2")" C-m

tmux select-layout -t "$SESSION" tiled
tmux attach -t "$SESSION"
