#!/usr/bin/env bash
#
# provider-up.sh — deploy the federation-autoscaler PROVIDER agent onto a
# provider cluster. Standalone (no Ansible, no cert-manager); run by the
# provider-cluster admin against their own cluster, using the join bundle the
# central admin issued for this cluster.
#
# It installs Liqo (control plane only — peering happens at runtime), creates
# the agent's mTLS client Secret from the bundle, deploys the provider agent,
# and points it at the broker. Prices / advertised-capacity / renewable are set
# afterwards from the provider CONSOLE (NodePort 30445), not by this script.
# Location is auto-discovered from the node IP (no region to set).
#
# Usage:
#   provider-up.sh --bundle <file.tgz> --cluster-id <id>
#                  [--tag <tag>] [--registry <reg>] [--kubeconfig <path>]
#                  [--namespace <ns>] [--public-endpoint <ip|host>]
#                  [--mock-eco-url <url>] [--mock-geo-url <url>]
#                  [--pod-cidr <cidr>] [--service-cidr <cidr>]
#                  [--liqo-provider <k3s|kubeadm|…>] [--skip-liqo]
#
#   --bundle       Join bundle from `central-up.sh join --cluster-id <id>`
#                  (broker URL + federation CA + this cluster's client cert/key).
#   --cluster-id   MUST equal the id the bundle was minted for (the client cert CN)
#                  and be unique + DNS-safe across the federation.
#   --mock-*-url   Optional carbon/coordinate endpoints (from mock-up.sh) enabling
#                  the eco/latency strategies; omit for a price-only provider.
#   --pod-cidr / --service-cidr  Passed to `liqoctl install`; MUST be globally
#                  non-overlapping across the federation (Liqo network plane).
#   --skip-liqo    Assume Liqo is already installed on this cluster.
#
# Assumes: an already-running Kubernetes cluster reachable via KUBECONFIG. Any
# missing CLI tools (kubectl, tar, liqoctl) are installed automatically — you
# only need Kubernetes + a kubeconfig, sudo, and outbound internet.

set -euo pipefail
# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

BUNDLE=""
CLUSTER_ID=""
KUBECONFIG_FLAG=""
PUBLIC_ENDPOINT=""
MOCK_ECO_URL=""
MOCK_GEO_URL=""
POD_CIDR=""
SERVICE_CIDR=""
LIQO_PROVIDER="k3s"
SKIP_LIQO=""

usage() { awk 'NR>2{ if (/^#/) { sub(/^# ?/,""); print } else { exit } }' "$0"; exit "${1:-0}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --bundle)          BUNDLE="$2"; shift 2 ;;
    --cluster-id)      CLUSTER_ID="$2"; shift 2 ;;
    --tag)             TAG="$2"; shift 2 ;;
    --registry)        REGISTRY="$2"; shift 2 ;;
    --kubeconfig)      KUBECONFIG_FLAG="$2"; shift 2 ;;
    --namespace)       NAMESPACE="$2"; shift 2 ;;
    --public-endpoint) PUBLIC_ENDPOINT="$2"; shift 2 ;;
    --mock-eco-url)    MOCK_ECO_URL="$2"; shift 2 ;;
    --mock-geo-url)    MOCK_GEO_URL="$2"; shift 2 ;;
    --pod-cidr)        POD_CIDR="$2"; shift 2 ;;
    --service-cidr)    SERVICE_CIDR="$2"; shift 2 ;;
    --liqo-provider)   LIQO_PROVIDER="$2"; shift 2 ;;
    --skip-liqo)       SKIP_LIQO=1; shift ;;
    -h|--help)         usage 0 ;;
    *)                 die "unknown argument: $1 (see --help)" ;;
  esac
done

[[ -n "$BUNDLE"     ]] || die "--bundle <file.tgz> is required (see --help)"
[[ -n "$CLUSTER_ID" ]] || die "--cluster-id <id> is required"
[[ -f "$BUNDLE"     ]] || die "--bundle file not found: $BUNDLE"
ensure_tools kubectl tar
use_kubeconfig "$KUBECONFIG_FLAG"

# 1. Unpack the join bundle (broker-url + ca.crt + client.crt + client.key).
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
tar -xzf "$BUNDLE" -C "$work"
for f in broker-url ca.crt client.crt client.key; do
  [[ -s "$work/$f" ]] || die "bundle is missing '$f' — re-issue it with central-up.sh join"
done
BROKER_URL="$(cat "$work/broker-url")"
log "Provider deploy — cluster-id ${CLUSTER_ID}, broker ${BROKER_URL}"

# 2. Liqo control plane (peering itself is on-demand at reservation time).
if [[ -n "$SKIP_LIQO" ]]; then
  warn "skipping Liqo install (--skip-liqo)"
elif kubectl get deploy liqo-controller-manager -n liqo >/dev/null 2>&1; then
  ok "Liqo already installed — skipping"
else
  ensure_tools liqoctl
  log "Installing Liqo (cluster-id ${CLUSTER_ID})"
  liqo_args=(install "$LIQO_PROVIDER" --cluster-id "$CLUSTER_ID" --timeout 10m)
  [[ -n "$POD_CIDR"     ]] && liqo_args+=(--pod-cidr "$POD_CIDR")
  [[ -n "$SERVICE_CIDR" ]] && liqo_args+=(--service-cidr "$SERVICE_CIDR")
  liqoctl "${liqo_args[@]}"
fi

# 3. Namespace + the agent's mTLS client Secret (before the overlay mounts it).
ensure_namespace
apply_tls_secret "agent-client-cert" "$work/client.crt" "$work/client.key" "$work/ca.crt"

# 4. Provider agent overlay (--role=provider, console 30445, no cert-manager).
log "Applying provider agent overlay"
apply_overlay "${FA_REPO_ROOT}/config/standalone/agent-provider" "agent"

# 5. Point the agent at the broker + mocks, then restart to pick up the config.
log "Configuring agent-config (broker + mock URLs)"
kubectl -n "$NAMESPACE" patch configmap agent-config --type merge -p \
  "$(printf '{"data":{"clusterId":"%s","liqoClusterId":"%s","brokerUrl":"%s","mockEcoUrl":"%s","mockGeoUrl":"%s"}}' \
      "$CLUSTER_ID" "$CLUSTER_ID" "$BROKER_URL" "$MOCK_ECO_URL" "$MOCK_GEO_URL")" >/dev/null
kubectl -n "$NAMESPACE" rollout restart deploy/agent
kubectl -n "$NAMESPACE" rollout status deploy/agent --timeout=120s

# ----------------------------------------------------------------------------
# Done
# ----------------------------------------------------------------------------
ep="${PUBLIC_ENDPOINT:-<provider-node-ip>}"
echo
ok "Provider '${CLUSTER_ID}' is up and advertising to the broker."
cat <<EOF

Set this provider's unit prices, advertised CPU/RAM %, and renewable flag from
its config console (or apply the agent-prices / agent-capacity / agent-renewable
ConfigMaps by hand). Location is auto-discovered from the node IP:

  http://${ep}:30445/

Within ~30 s the provider appears on the broker dashboard.
EOF
