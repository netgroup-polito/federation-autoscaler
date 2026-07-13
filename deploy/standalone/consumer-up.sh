#!/usr/bin/env bash
#
# consumer-up.sh — deploy the federation-autoscaler CONSUMER stack onto a
# consumer cluster: the consumer agent, the gRPC server, and Cluster Autoscaler.
# Standalone (no Ansible, no cert-manager); run by the consumer-cluster admin
# using the join bundle the central admin issued for this cluster.
#
# Two trust domains, both minted with openssl here:
#   - agent <-> broker: the agent's client cert comes from the join bundle
#     (signed by the FEDERATION CA on central; CN = cluster-id).
#   - Cluster Autoscaler <-> gRPC server: an in-cluster mTLS pair signed by a
#     CONSUMER-LOCAL CA generated on the fly (unrelated to the federation CA).
#
# Placement policy is set afterwards from the consumer CONSOLE (NodePort 30445),
# not by this script; the workload can be applied from there too. Location is
# auto-discovered from the node IP (no region to set).
#
# Usage:
#   consumer-up.sh --bundle <file.tgz> --cluster-id <id>
#                  [--tag <tag>] [--registry <reg>] [--kubeconfig <path>]
#                  [--namespace <ns>] [--public-endpoint <ip|host>]
#                  [--mock-geo-url <url>]
#                  [--pod-cidr <cidr>] [--service-cidr <cidr>]
#                  [--liqo-provider <k3s|kubeadm|…>] [--skip-liqo]
#                  [--ca-image <img>] [--scale-down-unneeded-time <dur>]
#                  [--skip-liqo-dashboard] [--install-metrics-server]
#                  [--liqo-dashboard-dir <dir>]
#
#   --bundle       Join bundle from `central-up.sh join --cluster-id <id>`.
#   --cluster-id   MUST equal the id the bundle was minted for (the cert CN);
#                  unique + DNS-safe across the federation.
#   --mock-geo-url Optional coordinate endpoint (from mock-up.sh) enabling the
#                  latency strategy for this consumer; omit to opt out.
#   --pod-cidr / --service-cidr  Passed to `liqoctl install`; MUST be globally
#                  non-overlapping across the federation.
#   --scale-down-unneeded-time  CA scale-down window (default 5m; use ~1m for a
#                  snappy demo, longer for production).
#
# By default this also installs the ArubaKube Liqo dashboard via Helm (peerings /
# virtual nodes / offloaded pods, Ingress host liqo-dashboard.local) — pass
# --skip-liqo-dashboard to omit it. k3s bundles a metrics-server; pass
# --install-metrics-server only on a cluster that lacks one.
#
# Assumes: an already-running Kubernetes cluster reachable via KUBECONFIG. Any
# missing CLI tools (kubectl, openssl, tar, liqoctl, helm, git) are installed
# automatically — you only need Kubernetes + a kubeconfig, sudo, and outbound
# internet.

set -euo pipefail
# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

BUNDLE=""
CLUSTER_ID=""
KUBECONFIG_FLAG=""
PUBLIC_ENDPOINT=""
MOCK_GEO_URL=""
POD_CIDR=""
SERVICE_CIDR=""
LIQO_PROVIDER="k3s"
SKIP_LIQO=""
CA_IMAGE="registry.k8s.io/autoscaling/cluster-autoscaler:v1.32.0"
SCALE_DOWN_UNNEEDED_TIME="5m"
SKIP_LIQO_DASHBOARD=""
INSTALL_METRICS_SERVER=""
LIQO_DASHBOARD_DIR="${HOME}/liqo-dashboard"

usage() { awk 'NR>2{ if (/^#/) { sub(/^# ?/,""); print } else { exit } }' "$0"; exit "${1:-0}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --bundle)                    BUNDLE="$2"; shift 2 ;;
    --cluster-id)                CLUSTER_ID="$2"; shift 2 ;;
    --tag)                       TAG="$2"; shift 2 ;;
    --registry)                  REGISTRY="$2"; shift 2 ;;
    --kubeconfig)                KUBECONFIG_FLAG="$2"; shift 2 ;;
    --namespace)                 NAMESPACE="$2"; shift 2 ;;
    --public-endpoint)           PUBLIC_ENDPOINT="$2"; shift 2 ;;
    --mock-geo-url)              MOCK_GEO_URL="$2"; shift 2 ;;
    --pod-cidr)                  POD_CIDR="$2"; shift 2 ;;
    --service-cidr)              SERVICE_CIDR="$2"; shift 2 ;;
    --liqo-provider)             LIQO_PROVIDER="$2"; shift 2 ;;
    --skip-liqo)                 SKIP_LIQO=1; shift ;;
    --ca-image)                  CA_IMAGE="$2"; shift 2 ;;
    --scale-down-unneeded-time)  SCALE_DOWN_UNNEEDED_TIME="$2"; shift 2 ;;
    --skip-liqo-dashboard)       SKIP_LIQO_DASHBOARD=1; shift ;;
    --install-metrics-server)    INSTALL_METRICS_SERVER=1; shift ;;
    --liqo-dashboard-dir)        LIQO_DASHBOARD_DIR="$2"; shift 2 ;;
    -h|--help)                   usage 0 ;;
    *)                           die "unknown argument: $1 (see --help)" ;;
  esac
done

[[ -n "$BUNDLE"     ]] || die "--bundle <file.tgz> is required (see --help)"
[[ -n "$CLUSTER_ID" ]] || die "--cluster-id <id> is required"
[[ -f "$BUNDLE"     ]] || die "--bundle file not found: $BUNDLE"
ensure_tools kubectl openssl tar
use_kubeconfig "$KUBECONFIG_FLAG"

GRPC_ADDR="grpc-server.${NAMESPACE}.svc.cluster.local:9443"

# install_liqo_dashboard — port of the Ansible liqo_dashboard role: clone the
# ArubaKube chart, helm upgrade --install it, then top up its ClusterRole with
# the few reads the backend needs (otherwise the UI 403s). Ingress host
# liqo-dashboard.local via the cluster's ingress controller (k3s Traefik).
install_liqo_dashboard() {
  ensure_tools helm git
  local repo="https://github.com/ArubaKube/liqo-dashboard.git"
  local dir="$LIQO_DASHBOARD_DIR"
  local release="liqo-dashboard" ns="liqo-dashboard" tag="main"
  local cr="liqo-dashboard-liqo-dashboard"   # <release>-<chart>

  if [[ -n "$INSTALL_METRICS_SERVER" ]]; then
    log "Applying upstream metrics-server"
    kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
  fi

  log "Fetching the liqo-dashboard chart (${repo} @ main)"
  if [[ -d "$dir/.git" ]]; then
    git -C "$dir" fetch --depth 1 origin main
    git -C "$dir" reset --hard FETCH_HEAD
  else
    git clone --depth 1 --branch main "$repo" "$dir"
  fi

  log "Installing the liqo-dashboard Helm release"
  helm upgrade --install "$release" \
    --namespace "$ns" --create-namespace \
    "${dir}/deployments/liqo-dashboard" \
    --set image.tag="$tag" --wait --timeout 5m

  # The upstream ClusterRole under-scopes a few reads; append them if absent
  # (idempotent — re-running never stacks duplicates).
  local res grp
  res="$(kubectl get clusterrole "$cr" -o jsonpath='{.rules[*].resources}' 2>/dev/null || true)"
  grp="$(kubectl get clusterrole "$cr" -o jsonpath='{.rules[*].apiGroups}' 2>/dev/null || true)"
  grep -qw configmaps <<<"$res" || kubectl patch clusterrole "$cr" --type=json \
    -p='[{"op":"add","path":"/rules/-","value":{"apiGroups":[""],"resources":["configmaps"],"verbs":["get","list","watch"]}}]'
  grep -qw deployments <<<"$res" || kubectl patch clusterrole "$cr" --type=json \
    -p='[{"op":"add","path":"/rules/-","value":{"apiGroups":["apps"],"resources":["deployments"],"verbs":["get","list"]}}]'
  grep -q 'ipam.liqo.io' <<<"$grp" || kubectl patch clusterrole "$cr" --type=json \
    -p='[{"op":"add","path":"/rules/-","value":{"apiGroups":["ipam.liqo.io"],"resources":["networks"],"verbs":["get","list"]}}]'
  ok "liqo-dashboard installed (Ingress host liqo-dashboard.local)"
}

# 1. Unpack the join bundle (broker-url + ca.crt + client.crt + client.key).
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
tar -xzf "$BUNDLE" -C "$work"
for f in broker-url ca.crt client.crt client.key; do
  [[ -s "$work/$f" ]] || die "bundle is missing '$f' — re-issue it with central-up.sh join"
done
BROKER_URL="$(cat "$work/broker-url")"
log "Consumer deploy — cluster-id ${CLUSTER_ID}, broker ${BROKER_URL}"

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

# 3. CRDs (VirtualNodeState + ConsumerPolicy are used on the consumer) + namespace.
log "Applying federation CRDs"
kubectl apply -k "${FA_REPO_ROOT}/config/crd"
ensure_namespace

# 4. agent <-> broker: client Secret straight from the bundle (federation CA).
apply_tls_secret "agent-client-cert" "$work/client.crt" "$work/client.key" "$work/ca.crt"

# 5. CA <-> gRPC: a consumer-LOCAL CA signs both ends (in-cluster only).
log "Minting the consumer-internal CA<->gRPC mTLS pair"
gen_ca "$work/ica" "consumer-internal-ca" >/dev/null
sign_leaf "$work/ica" "$work/grpc" "grpc-server.${NAMESPACE}.svc" serverAuth \
  "grpc-server" "grpc-server.${NAMESPACE}" "grpc-server.${NAMESPACE}.svc" "grpc-server.${NAMESPACE}.svc.cluster.local"
sign_leaf "$work/ica" "$work/ca-client" "cluster-autoscaler" clientAuth
apply_tls_secret "grpc-server-cert" "$work/grpc.crt" "$work/grpc.key" "$work/ica/ca.crt"
apply_tls_secret "cluster-autoscaler-client-cert" "$work/ca-client.crt" "$work/ca-client.key" "$work/ica/ca.crt"

# 6. Consumer agent + gRPC server overlays (no cert-manager, console 30445).
log "Applying consumer agent overlay"
apply_overlay "${FA_REPO_ROOT}/config/standalone/agent-consumer" "agent"
log "Applying gRPC server overlay"
apply_overlay "${FA_REPO_ROOT}/config/standalone/grpc-server" "grpc-server"

# 7. Point the agent at the broker + mock-geo (consumers use mock-geo only).
log "Configuring agent-config (broker + mock-geo URL)"
kubectl -n "$NAMESPACE" patch configmap agent-config --type merge -p \
  "$(printf '{"data":{"clusterId":"%s","liqoClusterId":"%s","brokerUrl":"%s","mockEcoUrl":"","mockGeoUrl":"%s"}}' \
      "$CLUSTER_ID" "$CLUSTER_ID" "$BROKER_URL" "$MOCK_GEO_URL")" >/dev/null

# 8. Cluster Autoscaler (externalgrpc -> gRPC server) + Liqo NamespaceOffloading.
log "Applying Cluster Autoscaler"
sed -e "s#__NAMESPACE__#${NAMESPACE}#g" \
    -e "s#__CA_IMAGE__#${CA_IMAGE}#g" \
    -e "s#__GRPC_ADDR__#${GRPC_ADDR}#g" \
    -e "s#__SCALE_DOWN_UNNEEDED_TIME__#${SCALE_DOWN_UNNEEDED_TIME}#g" \
    "${FA_STANDALONE_DIR}/manifests/cluster-autoscaler.yaml" | kubectl apply -f -
log "Stamping Liqo NamespaceOffloading for the default namespace"
kubectl apply -f "${FA_STANDALONE_DIR}/manifests/namespaceoffloading.yaml"

# 9. Restart the agent (to pick up agent-config) and wait for everything.
kubectl -n "$NAMESPACE" rollout restart deploy/agent
log "Waiting for agent / gRPC server / Cluster Autoscaler to become Available"
kubectl -n "$NAMESPACE" rollout status deploy/agent --timeout=120s
kubectl -n "$NAMESPACE" rollout status deploy/grpc-server --timeout=120s
kubectl -n "$NAMESPACE" rollout status deploy/cluster-autoscaler --timeout=120s

# 10. Liqo dashboard (peerings / virtual nodes / offloaded pods) — as the Ansible
#     liqo_dashboard role installs it on consumers.
if [[ -n "$SKIP_LIQO_DASHBOARD" ]]; then
  warn "skipping the Liqo dashboard (--skip-liqo-dashboard)"
else
  install_liqo_dashboard
fi

# ----------------------------------------------------------------------------
# Done
# ----------------------------------------------------------------------------
ep="${PUBLIC_ENDPOINT:-<consumer-node-ip>}"
echo
ok "Consumer '${CLUSTER_ID}' is up."
cat <<EOF

Drive it from the consumer config console:

  http://${ep}:30445/

  - pick the placement policy (Standard / Price / Eco / Latency)
  - flip the workload switch ON to scale up / OFF to scale down
  (location is auto-discovered from the node IP, shown read-only)

Watch the broker dashboard (http://<central-ip>:30444/) to see which provider
each policy selects.
EOF
if [[ -z "$SKIP_LIQO_DASHBOARD" ]]; then
  cat <<EOF

Liqo dashboard (peerings, virtual nodes, offloaded pods):
  add '${ep} liqo-dashboard.local' to your machine's hosts file, then browse
  http://liqo-dashboard.local
EOF
fi
