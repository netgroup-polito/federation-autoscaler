#!/usr/bin/env bash
#
# generate-test-certs.sh — Create a disposable test-only CA and per-agent
# mTLS client certificates for the broker scalability test.
#
# Each logical agent gets a cert with CN=scaltest-{provider|consumer}-{NNN}.
# These are NOT production federation certs — use deploy/standalone/central-up.sh
# join for real environments.
#
# Usage:
#   ./generate-test-certs.sh --consumers 50 --providers 100 --out-dir certs/run-001
#
# Requires: openssl

set -euo pipefail

# ---- Git-Bash / MSYS2 (Windows) compatibility ----
# Problem 1: MSYS2 rewrites any argument starting with "/" as a Windows path.
#   "/CN=foo" becomes "C:/Program Files/Git/CN=foo".
#   Fix: set MSYS_NO_PATHCONV=1 — but ONLY around openssl calls that use
#   -subj, not globally, because global suppression also breaks mktemp paths.
# Problem 2: mingw64's openssl hangs on `openssl req` when stdin is a
#   terminal. Fix: redirect stdin from /dev/null and pass -batch.
# Problem 3: mktemp returns /tmp/... which MSYS2 normally translates to a
#   real Windows path, but MSYS_NO_PATHCONV=1 suppresses that too, so
#   openssl gets a literal /tmp/... that doesn't exist on Windows.
#   Fix: use temp files inside $OUT_DIR instead of mktemp.

CONSUMERS=1
PROVIDERS=1
OUT_DIR=""
BROKER_SAN="localhost"

usage() {
  echo "Usage: $0 --consumers N --providers N --out-dir <dir> [--broker-san <host>]"
  echo ""
  echo "  --consumers N       Number of consumer client certs to generate (default: 1)"
  echo "  --providers N       Number of provider client certs to generate (default: 1)"
  echo "  --out-dir <dir>     Output directory for certs (required)"
  echo "  --broker-san <host> SAN for the server cert (default: localhost)"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --consumers)   CONSUMERS="$2"; shift 2 ;;
    --providers)   PROVIDERS="$2"; shift 2 ;;
    --out-dir)     OUT_DIR="$2"; shift 2 ;;
    --broker-san)  BROKER_SAN="$2"; shift 2 ;;
    -h|--help)     usage 0 ;;
    *)             echo "Unknown flag: $1" >&2; usage 1 ;;
  esac
done

[[ -n "$OUT_DIR" ]] || { echo "ERROR: --out-dir is required" >&2; usage 1; }
command -v openssl >/dev/null 2>&1 || { echo "ERROR: openssl not found" >&2; exit 1; }

mkdir -p "$OUT_DIR"

# --- CA ---
if [[ -s "$OUT_DIR/ca.key" && -s "$OUT_DIR/ca.crt" ]]; then
  echo "Reusing existing CA in $OUT_DIR"
else
  echo "Generating test CA..."
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$OUT_DIR/ca.key" 2>/dev/null
  chmod 600 "$OUT_DIR/ca.key"
  MSYS_NO_PATHCONV=1 openssl req -x509 -new -key "$OUT_DIR/ca.key" -sha256 -days 365 \
    -subj "/CN=scalability-test-ca" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -out "$OUT_DIR/ca.crt" </dev/null 2>/dev/null
  echo "  CA: $OUT_DIR/ca.crt"
fi

# --- Helper: sign a leaf cert ---
sign_leaf() {
  local cn="$1" eku="$2" out_prefix="$3"
  shift 3
  local ext="${out_prefix}._ext" csr="${out_prefix}._csr"
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
    -out "${out_prefix}.key" 2>/dev/null
  chmod 600 "${out_prefix}.key"
  {
    echo "basicConstraints=critical,CA:FALSE"
    echo "keyUsage=critical,digitalSignature,keyEncipherment"
    echo "extendedKeyUsage=${eku}"
    if [[ $# -gt 0 ]]; then
      local sans=""
      for s in "$@"; do
        if [[ "$s" =~ ^[0-9]+(\.[0-9]+){3}$ ]]; then sans+="IP:${s},"; else sans+="DNS:${s},"; fi
      done
      echo "subjectAltName=${sans%,}"
    fi
  } > "$ext"
  MSYS_NO_PATHCONV=1 openssl req -new -key "${out_prefix}.key" \
    -subj "/CN=${cn}" -batch -out "$csr" </dev/null 2>/dev/null
  openssl x509 -req -in "$csr" -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" \
    -CAcreateserial -days 365 -sha256 -extfile "$ext" \
    -out "${out_prefix}.crt" 2>/dev/null
  rm -f "$ext" "$csr"
}

# --- Server cert (for local test broker) ---
echo "Generating server cert (SAN: $BROKER_SAN, localhost, 127.0.0.1)..."
sign_leaf "broker.test" "serverAuth" "$OUT_DIR/server" \
  "$BROKER_SAN" "localhost" "127.0.0.1"
echo "  Server: $OUT_DIR/server.crt"

# --- Provider client certs ---
echo "Generating $PROVIDERS provider cert(s)..."
for i in $(seq 1 "$PROVIDERS"); do
  id="$(printf 'scaltest-provider-%03d' "$i")"
  sign_leaf "$id" "clientAuth" "$OUT_DIR/$id"
done
echo "  Providers: $OUT_DIR/scaltest-provider-{001..$( printf '%03d' "$PROVIDERS" )}.{crt,key}"

# --- Consumer client certs ---
echo "Generating $CONSUMERS consumer cert(s)..."
for i in $(seq 1 "$CONSUMERS"); do
  id="$(printf 'scaltest-consumer-%03d' "$i")"
  sign_leaf "$id" "clientAuth" "$OUT_DIR/$id"
done
echo "  Consumers: $OUT_DIR/scaltest-consumer-{001..$( printf '%03d' "$CONSUMERS" )}.{crt,key}"

echo ""
echo "Done. $((PROVIDERS + CONSUMERS)) client certs + 1 server cert + 1 CA in $OUT_DIR"
echo "CA is test-only (CN=scalability-test-ca, 365-day validity). Do not use in production."
