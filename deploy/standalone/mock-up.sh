#!/usr/bin/env bash
#
# mock-up.sh — deploy the demo mock services (mock-eco + mock-geo) onto the
# dedicated mock cluster. Standalone (no Ansible); run by the mock-cluster admin.
#
# The mocks stand in for real carbon-intensity (mock-eco, GET /carbon?region=)
# and geo-IP (mock-geo, GET /json/<ip>) APIs that feed the eco and
# latency placement strategies. Provider/consumer agents fetch from them over
# the network; the broker never calls them. In production you would skip this
# script and instead point the agents' --mock-eco-url/--mock-geo-url at real
# services.
#
# The mock cluster needs NOTHING else: no cert-manager, no Liqo, no CRDs, no TLS.
#
# Usage:
#   mock-up.sh [--public-endpoint <ip|host>] [--tag <tag>] [--registry <reg>]
#              [--kubeconfig <path>] [--namespace <ns>]
#
#   --public-endpoint  Routable IP/hostname of this mock cluster's node, used only
#                      to print the URLs you pass to the agents. Optional.
#   --tag / --registry Image coordinate <registry>/federation-autoscaler-mock-*:<tag>
#                      (default registry docker.io/kazem26, tag latest).
#
# Assumes: an already-running Kubernetes cluster reachable via KUBECONFIG. kubectl
# is installed automatically if missing — you only need Kubernetes + a kubeconfig,
# sudo, and outbound internet.

set -euo pipefail
# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

PUBLIC_ENDPOINT=""
KUBECONFIG_FLAG=""

usage() { awk 'NR>2{ if (/^#/) { sub(/^# ?/,""); print } else { exit } }' "$0"; exit "${1:-0}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --public-endpoint) PUBLIC_ENDPOINT="$2"; shift 2 ;;
    --tag)             TAG="$2"; shift 2 ;;
    --registry)        REGISTRY="$2"; shift 2 ;;
    --kubeconfig)      KUBECONFIG_FLAG="$2"; shift 2 ;;
    --namespace)       NAMESPACE="$2"; shift 2 ;;
    -h|--help)         usage 0 ;;
    *)                 die "unknown argument: $1 (see --help)" ;;
  esac
done

ensure_tools kubectl
use_kubeconfig "$KUBECONFIG_FLAG"

log "Mock deploy — images ${REGISTRY}/federation-autoscaler-mock-{eco,geo}:${TAG}"

# The overlays carry the Namespace + Service (NodePort 30081/30080) + Deployment.
log "Applying mock-eco (carbon) overlay"
apply_overlay "${FA_REPO_ROOT}/config/standalone/mock-eco" "mock-eco"
log "Applying mock-geo (coordinates) overlay"
apply_overlay "${FA_REPO_ROOT}/config/standalone/mock-geo" "mock-geo"

log "Waiting for the mock Deployments to become Available"
kubectl -n "$NAMESPACE" rollout status deploy/mock-eco --timeout=120s
kubectl -n "$NAMESPACE" rollout status deploy/mock-geo --timeout=120s

# ----------------------------------------------------------------------------
# Done
# ----------------------------------------------------------------------------
ep="${PUBLIC_ENDPOINT:-<mock-node-ip>}"
echo
ok "Mock services are up."
cat <<EOF

Pass these URLs to each provider (both) and the consumer (geo only):

  mock-eco (carbon):      http://${ep}:30081
  mock-geo (coordinates): http://${ep}:30080

  provider-up.sh … --mock-eco-url http://${ep}:30081 --mock-geo-url http://${ep}:30080
  consumer-up.sh … --mock-geo-url http://${ep}:30080
EOF
