#!/usr/bin/env bash
#
# common.sh — shared helpers for the per-cluster standalone deploy scripts
# (central-up.sh / provider-up.sh / consumer-up.sh / mock-up.sh).
#
# Sourced, not executed. Provides: logging, shared-flag defaults, tool checks,
# an openssl mini-CA (gen_ca / sign_leaf), a TLS-Secret applier, and a
# kustomize-overlay applier that retags the component image on the fly.
#
# Design invariants (see deploy/standalone/README.md):
#   - Targets an ALREADY-RUNNING cluster via KUBECONFIG / --kubeconfig; never
#     installs k3s.
#   - No cert-manager anywhere: certs are minted here with openssl and written
#     straight into Secrets.
#   - The federation CA private key lives only on the central admin's machine
#     (--ca-dir); participants receive a pre-signed client cert in a join bundle.

# ----------------------------------------------------------------------------
# Paths
# ----------------------------------------------------------------------------
# deploy/standalone/common.sh  ->  repo root is two levels up.
FA_STANDALONE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC2034  # consumed by the sourcing up-scripts
FA_REPO_ROOT="$(cd "${FA_STANDALONE_DIR}/../.." && pwd)"

# ----------------------------------------------------------------------------
# Shared flag defaults (override via each script's flags)
# ----------------------------------------------------------------------------
NAMESPACE="${NAMESPACE:-federation-autoscaler-system}"
REGISTRY="${REGISTRY:-docker.io/kazem26}"
TAG="${TAG:-latest}"

# ----------------------------------------------------------------------------
# Logging (matches deploy/ansible/scripts/demo-up.sh)
# ----------------------------------------------------------------------------
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✔\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m  ! \033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# retry <max_attempts> <sleep_seconds> <cmd...> — run cmd, retrying on
# failure up to max_attempts times with a fixed sleep between attempts.
# Meant for steps that touch external network resources (e.g. `liqoctl
# install` pulling release assets from GitHub), which can transiently fail —
# especially under the background load a large comparative-eco/latency run
# with many already-running Kind clusters generates on the host's own DNS
# resolver / outbound network.
retry() {
  local max="$1" sleep_s="$2"; shift 2
  local attempt=1
  until "$@"; do
    if (( attempt >= max )); then
      return 1
    fi
    warn "command failed (attempt ${attempt}/${max}), retrying in ${sleep_s}s: $*"
    sleep "$sleep_s"
    attempt=$((attempt + 1))
  done
}

# retry_liqo_install <liqoctl install args...> — like retry(), but purges any
# partial install between attempts. A `liqoctl install` that times out (e.g.
# under the resource pressure of many already-running Kind clusters) can
# leave the 'liqo' namespace populated with objects no Helm release tracks
# anymore (liqoctl's own auto-rollback removes the release but not every
# object). Without a purge, every subsequent `liqoctl install` fails
# instantly with `"liqo" has no deployed releases` instead of getting a real
# fresh attempt, so the retry loop burns its whole budget doing nothing —
# observed at ~50-cluster scale where the first install alone timed out.
retry_liqo_install() {
  # max=2, not 3: each attempt can now run up to --timeout (20m), and this
  # already nests inside retryDeploy's own 3 outer attempts (federation-tests/testlib) —
  # 3x3 at 20m each would let one stuck cluster stall for hours before the
  # run finally gives up on it.
  local max=2 sleep_s=15
  local attempt=1
  until liqoctl "$@"; do
    if (( attempt >= max )); then
      return 1
    fi
    warn "liqoctl install failed (attempt ${attempt}/${max}) — purging the partial install, retrying in ${sleep_s}s"
    liqoctl uninstall --skip-confirm >/dev/null 2>&1 || true
    if kubectl get namespace liqo >/dev/null 2>&1; then
      kubectl delete namespace liqo --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
      if kubectl get namespace liqo >/dev/null 2>&1; then
        # Still there: it's stuck Terminating, almost always because some Liqo
        # CR's finalizer never got cleared (its controller pod is already
        # gone). Force-clear the namespace's own finalizer so it disappears
        # regardless — the standard escape hatch for a stuck namespace.
        warn "namespace 'liqo' stuck terminating — force-clearing its finalizer"
        kubectl patch namespace liqo --type=merge -p '{"spec":{"finalizers":[]}}' --subresource=finalize >/dev/null 2>&1 || true
        for _ in $(seq 1 12); do
          kubectl get namespace liqo >/dev/null 2>&1 || break
          sleep 5
        done
      fi
    fi
    sleep "$sleep_s"
    attempt=$((attempt + 1))
  done
}

# ----------------------------------------------------------------------------
# Environment / tools
# ----------------------------------------------------------------------------
# use_kubeconfig <path> — export KUBECONFIG so every kubectl/liqoctl call honours it.
use_kubeconfig() { [[ -n "${1:-}" ]] && export KUBECONFIG="$1"; }

# Tool versions installed when missing (kept in lock-step with deploy/ansible).
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.32.5}"
LIQOCTL_VERSION="${LIQOCTL_VERSION:-v1.1.2}"

_APT_UPDATED=""
_apt_install() {
  [[ -n "$_APT_UPDATED" ]] || { sudo apt-get update -y >/dev/null; _APT_UPDATED=1; }
  sudo apt-get install -y "$@" >/dev/null
}

# ensure_tools <tool...> — install any missing tool onto THIS machine (the one
# running the script; not the cluster). Assumes an Ubuntu host with sudo +
# outbound internet — the only thing you must have beforehand is Kubernetes +
# a kubeconfig. Mirrors deploy/ansible/scripts/demo-up.sh's installs.
ensure_tools() {
  local t tmp
  for t in "$@"; do
    command -v "$t" >/dev/null 2>&1 && continue
    warn "${t} not found — installing"
    case "$t" in
      curl|openssl|tar|git) _apt_install "$t" ;;
      kubectl)
        command -v curl >/dev/null 2>&1 || _apt_install curl
        tmp="$(mktemp -d)"
        curl -fsSL -o "$tmp/kubectl" "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
        sudo install -m0755 "$tmp/kubectl" /usr/local/bin/kubectl; rm -rf "$tmp" ;;
      liqoctl)
        command -v curl >/dev/null 2>&1 || _apt_install curl
        command -v tar  >/dev/null 2>&1 || _apt_install tar
        tmp="$(mktemp -d)"
        curl -fsSL "https://github.com/liqotech/liqo/releases/download/${LIQOCTL_VERSION}/liqoctl-linux-amd64.tar.gz" \
          | tar -xz -C "$tmp" liqoctl
        sudo install -m0755 "$tmp/liqoctl" /usr/local/bin/liqoctl; rm -rf "$tmp" ;;
      helm)
        command -v curl >/dev/null 2>&1 || _apt_install curl
        tmp="$(mktemp -d)"
        curl -fsSL -o "$tmp/get-helm.sh" https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4
        chmod 700 "$tmp/get-helm.sh"; ( cd "$tmp" && ./get-helm.sh >/dev/null ); rm -rf "$tmp" ;;
      *) die "don't know how to install '${t}'" ;;
    esac
    command -v "$t" >/dev/null 2>&1 || die "failed to install ${t}"
    ok "installed ${t}"
  done
}

# ensure_namespace — create NAMESPACE if absent (idempotent).
ensure_namespace() {
  kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

# ----------------------------------------------------------------------------
# openssl mini-CA
# ----------------------------------------------------------------------------
# _san_list <san...> — render an openssl subjectAltName value, choosing IP: vs
# DNS: per entry (IPv4 detected by shape; hostnames and IPv6 -> DNS:).
_san_list() {
  local out="" e
  for e in "$@"; do
    if [[ "$e" =~ ^[0-9]+(\.[0-9]+){3}$ ]]; then out+="IP:${e},"; else out+="DNS:${e},"; fi
  done
  printf '%s' "${out%,}"
}

# gen_ca <dir> [cn] — create <dir>/ca.key + <dir>/ca.crt if absent (idempotent).
# ECDSA P-256, 10-year CA. The private key is chmod 600 and never leaves <dir>.
gen_ca() {
  local dir="$1" cn="${2:-federation-autoscaler-ca}"
  mkdir -p "$dir"; chmod 700 "$dir"
  if [[ -s "$dir/ca.key" && -s "$dir/ca.crt" ]]; then ok "reusing CA in $dir"; return; fi
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$dir/ca.key" 2>/dev/null
  chmod 600 "$dir/ca.key"
  openssl req -x509 -new -key "$dir/ca.key" -sha256 -days 3650 -subj "/CN=${cn}" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -out "$dir/ca.crt" 2>/dev/null
  ok "generated CA ${dir}/ca.crt (CN=${cn})"
}

# sign_leaf <ca_dir> <out_prefix> <cn> <serverAuth|clientAuth> [san...]
# Writes <out_prefix>.key and <out_prefix>.crt, signed by <ca_dir>/ca.{crt,key}.
sign_leaf() {
  local ca_dir="$1" out="$2" cn="$3" eku="$4"; shift 4
  local sans=("$@") ext csr
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "${out}.key" 2>/dev/null
  chmod 600 "${out}.key"
  ext="$(mktemp)"; csr="$(mktemp)"
  {
    echo "basicConstraints=critical,CA:FALSE"
    echo "keyUsage=critical,digitalSignature,keyEncipherment"
    echo "extendedKeyUsage=${eku}"
    ((${#sans[@]})) && printf 'subjectAltName=%s\n' "$(_san_list "${sans[@]}")"
  } > "$ext"
  openssl req -new -key "${out}.key" -subj "/CN=${cn}" -out "$csr" 2>/dev/null
  openssl x509 -req -in "$csr" -CA "${ca_dir}/ca.crt" -CAkey "${ca_dir}/ca.key" \
    -CAcreateserial -days 825 -sha256 -extfile "$ext" -out "${out}.crt" 2>/dev/null
  rm -f "$ext" "$csr"
}

# ----------------------------------------------------------------------------
# Kubernetes appliers
# ----------------------------------------------------------------------------
# apply_tls_secret <name> <crt> <key> <ca> — idempotent generic Secret carrying
# tls.crt / tls.key / ca.crt (the key layout every fa component mounts).
apply_tls_secret() {
  local name="$1" crt="$2" key="$3" ca="$4"
  kubectl create secret generic "$name" -n "$NAMESPACE" \
    --from-file=tls.crt="$crt" --from-file=tls.key="$key" --from-file=ca.crt="$ca" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  ok "applied Secret ${name} (-n ${NAMESPACE})"
}

# apply_overlay <overlay_dir> <component> — render a standalone kustomize overlay,
# retag the component image to ${REGISTRY}/federation-autoscaler-<component>:${TAG},
# and apply. <component> is one of broker|agent|grpc-server|mock-eco|mock-geo.
apply_overlay() {
  local dir="$1" comp="$2"
  kubectl kustomize "$dir" \
    | sed "s#image: federation-autoscaler/${comp}:latest#image: ${REGISTRY}/federation-autoscaler-${comp}:${TAG}#g" \
    | kubectl apply -f -
}
