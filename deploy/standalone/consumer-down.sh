#!/usr/bin/env bash
#
# consumer-down.sh — remove the consumer stack (agent + gRPC server + Cluster
# Autoscaler + Liqo dashboard + NamespaceOffloading). Does NOT touch the node.
#
# Usage:
#   consumer-down.sh [--kubeconfig <path>] [--namespace <ns>]
#                    [--skip-liqo-dashboard] [--uninstall-liqo] [--delete-crds]
#
#   --skip-liqo-dashboard  Don't try to helm-uninstall the Liqo dashboard.
#   --uninstall-liqo       Also unpeer + `liqoctl uninstall` (best-effort).
#   --delete-crds          Also remove the federation CRDs (cluster-scoped).

set -euo pipefail
# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

KUBECONFIG_FLAG=""
SKIP_LIQO_DASHBOARD=""
UNINSTALL_LIQO=""
DELETE_CRDS=""
usage() { awk 'NR>2{ if (/^#/) { sub(/^# ?/,""); print } else { exit } }' "$0"; exit "${1:-0}"; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --kubeconfig)          KUBECONFIG_FLAG="$2"; shift 2 ;;
    --namespace)           NAMESPACE="$2"; shift 2 ;;
    --skip-liqo-dashboard) SKIP_LIQO_DASHBOARD=1; shift ;;
    --uninstall-liqo)      UNINSTALL_LIQO=1; shift ;;
    --delete-crds)         DELETE_CRDS=1; shift ;;
    -h|--help)             usage 0 ;;
    *)                     die "unknown argument: $1 (see --help)" ;;
  esac
done

ensure_tools kubectl
use_kubeconfig "$KUBECONFIG_FLAG"

log "Deleting Cluster Autoscaler"
kubectl -n "$NAMESPACE" delete deploy/cluster-autoscaler sa/cluster-autoscaler \
  secret/cluster-autoscaler-cloud-config --ignore-not-found
kubectl delete clusterrolebinding/cluster-autoscaler --ignore-not-found

log "Deleting Liqo NamespaceOffloading (default namespace)"
kubectl delete -f "${FA_STANDALONE_DIR}/manifests/namespaceoffloading.yaml" --ignore-not-found

if [[ -z "$SKIP_LIQO_DASHBOARD" ]] && command -v helm >/dev/null 2>&1; then
  log "Uninstalling the Liqo dashboard"
  helm uninstall liqo-dashboard -n liqo-dashboard 2>/dev/null || true
  kubectl delete namespace liqo-dashboard --ignore-not-found
fi

if helm status kubeai -n kubeai >/dev/null 2>&1; then
  log "Uninstalling the AI (KubeAI and its model)"
  kubectl -n kubeai delete models.kubeai.org --all --ignore-not-found >/dev/null 2>&1 || true
  helm uninstall kubeai -n kubeai >/dev/null 2>&1 || true
  kubectl delete namespace kubeai --ignore-not-found
fi

log "Deleting the gRPC server and consumer agent overlays"
kubectl delete -k "${FA_REPO_ROOT}/config/standalone/grpc-server" --ignore-not-found
kubectl delete -k "${FA_REPO_ROOT}/config/standalone/agent-consumer" --ignore-not-found

log "Deleting namespace ${NAMESPACE} (cascades the agent/gRPC/CA Secrets)"
kubectl delete namespace "$NAMESPACE" --ignore-not-found

if [[ -n "$UNINSTALL_LIQO" ]]; then
  ensure_tools liqoctl
  warn "Uninstalling Liqo (best-effort)"
  liqoctl unpeer --skip-confirm 2>/dev/null || true
  liqoctl uninstall --skip-confirm 2>/dev/null || true
fi
if [[ -n "$DELETE_CRDS" ]]; then
  log "Deleting federation CRDs"
  kubectl delete -k "${FA_REPO_ROOT}/config/crd" --ignore-not-found
fi
ok "Consumer torn down."
