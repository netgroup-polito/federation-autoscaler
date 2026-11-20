#!/usr/bin/env bash
# kubeai-up.sh — run the consumer's AI on its own cluster with KubeAI.
#
# Installs KubeAI (Helm, open-webui off) and one model, sized by the options
# below, then prints the address and model name the consumer agent uses:
#   AI_URL=http://kubeai.<namespace>.svc/openai/v1
#   AI_MODEL=<name>
# Used by consumer-up.sh --kubeai and by the Ansible fa_kubeai role; it can
# also be run alone on an existing consumer cluster.
#
# Usage: kubeai-up.sh [--kubeconfig <path>] [--namespace kubeai]
#                     [--model phi4-mini:3.8b] [--hf-model microsoft/Phi-4-mini-instruct]
#                     [--name <model-name>] [--engine auto|ollama|vllm]
#                     [--cpus 2] [--memory 4Gi] [--gpus 1] [--replicas 1[:1]]
#                     [--chart-version 0.25.1] [--ollama-image ollama/ollama:0.40.15]
#                     [--no-wait]
#
#   --model        Ollama model for the CPU engine (default phi4-mini:3.8b, chosen
#                  on the lab benchmark; granite4.2:3b is a smaller alternative).
#   --hf-model     Hugging Face model for the vLLM engine (GPU).
#   --name         Name of the KubeAI Model, i.e. the model the agent asks for
#                  (default: --model without its tag, e.g. phi4-mini).
#   --engine       auto (default): vLLM when a node offers nvidia.com/gpu, Ollama
#                  on CPU otherwise. ollama / vllm force one.
#   --cpus, --memory  What one model copy reserves on CPU (default 2 and 4Gi;
#                  phi4-mini used about 4 GiB on the lab).
#   --gpus         GPUs per model copy with vLLM (default 1).
#   --replicas     MIN or MIN:MAX copies; KubeAI adds copies with load up to MAX
#                  and, with MIN 0, starts the first one on the first request.
#                  Each copy needs the room above, and so does KubeAI's rollout
#                  of a changed model (otherwise delete the old pod).
#   --no-wait      Do not wait for the model to answer.
#
# An Ollama model pod downloads its model when it starts (about 2.5 GB for
# phi4-mini); KubeAI caches model files only for vLLM. The script warns, but
# does not stop, when the cluster has less free CPU or memory than one copy
# reserves. Needs kubectl and helm; outbound internet for the chart and model.

set -euo pipefail
# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

KUBECONFIG_FLAG=""
AI_NAMESPACE="kubeai"
MODEL="phi4-mini:3.8b"
HF_MODEL="microsoft/Phi-4-mini-instruct"
NAME=""
ENGINE="auto"
CPUS="2"
MEMORY="4Gi"
GPUS="1"
REPLICAS="1"
CHART_VERSION="0.25.1"
OLLAMA_IMAGE="ollama/ollama:0.40.15"
WAIT=1

usage() { awk 'NR>2{ if (/^#/) { sub(/^# ?/,""); print } else { exit } }' "$0"; exit "${1:-0}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kubeconfig)     KUBECONFIG_FLAG="$2"; shift 2 ;;
    --namespace)      AI_NAMESPACE="$2"; shift 2 ;;
    --model)          MODEL="$2"; shift 2 ;;
    --hf-model)       HF_MODEL="$2"; shift 2 ;;
    --name)           NAME="$2"; shift 2 ;;
    --engine)         ENGINE="$2"; shift 2 ;;
    --cpus)           CPUS="$2"; shift 2 ;;
    --memory)         MEMORY="$2"; shift 2 ;;
    --gpus)           GPUS="$2"; shift 2 ;;
    --replicas)       REPLICAS="$2"; shift 2 ;;
    --chart-version)  CHART_VERSION="$2"; shift 2 ;;
    --ollama-image)   OLLAMA_IMAGE="$2"; shift 2 ;;
    --no-wait)        WAIT=""; shift ;;
    -h|--help)        usage 0 ;;
    *)                die "unknown argument: $1 (see --help)" ;;
  esac
done

case "$ENGINE" in auto|ollama|vllm) ;; *) die "--engine must be auto, ollama or vllm" ;; esac
MIN_REPLICAS="${REPLICAS%%:*}"
MAX_REPLICAS="${REPLICAS#*:}"
[[ "$MIN_REPLICAS" =~ ^[0-9]+$ && "$MAX_REPLICAS" =~ ^[0-9]+$ && "$MAX_REPLICAS" -ge 1 && "$MAX_REPLICAS" -ge "$MIN_REPLICAS" ]] \
  || die "--replicas must be MIN or MIN:MAX with MAX >= 1 and MAX >= MIN"
[[ -n "$NAME" ]] || NAME="$(printf '%s' "${MODEL%%:*}" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-' | sed 's/-*$//')"

ensure_tools kubectl helm
use_kubeconfig "$KUBECONFIG_FLAG"

# CPU in millicores and memory in MiB, from Kubernetes quantities.
to_milli() { awk -v q="$1" 'BEGIN{ if (q ~ /m$/) { sub(/m$/,"",q); print q+0 } else print (q+0)*1000 }'; }
to_mib() {
  awk -v q="$1" 'BEGIN{
    n=q; sub(/[A-Za-z]+$/,"",n); u=q; sub(/^[0-9.]+/,"",u)
    f["Ki"]=1/1024; f["Mi"]=1; f["Gi"]=1024; f["Ti"]=1048576
    f["k"]=1000/1048576; f["M"]=1000000/1048576; f["G"]=1000000000/1048576; f[""]=1/1048576
    printf "%d", n*f[u] }'
}

# How much the cluster has left: allocatable minus what running pods reserve
# (outside the AI namespace, which this script replaces anyway).
free_room() {
  local alloc_cpu=0 alloc_mem=0 req_cpu=0 req_mem=0
  while read -r c m; do
    alloc_cpu=$(( alloc_cpu + $(to_milli "$c") )); alloc_mem=$(( alloc_mem + $(to_mib "$m") ))
  done < <(kubectl get nodes -o jsonpath='{range .items[*]}{.status.allocatable.cpu} {.status.allocatable.memory}{"\n"}{end}')
  while read -r ns c m; do
    [[ "$ns" == "$AI_NAMESPACE" ]] && continue
    [[ -n "$c" && "$c" != "<none>" ]] && req_cpu=$(( req_cpu + $(to_milli "$c") ))
    [[ -n "$m" && "$m" != "<none>" ]] && req_mem=$(( req_mem + $(to_mib "$m") ))
  done < <(kubectl get pods -A --field-selector=status.phase!=Succeeded,status.phase!=Failed --no-headers \
             -o custom-columns='NS:.metadata.namespace,CPU:.spec.containers[*].resources.requests.cpu,MEM:.spec.containers[*].resources.requests.memory' \
             | awk '{ n=split($2,c,","); split($3,m,","); for (i=1;i<=n;i++) print $1, c[i], m[i] }')
  echo "$(( alloc_cpu - req_cpu )) $(( alloc_mem - req_mem ))"
}

GPU_COUNT="$(kubectl get nodes -o jsonpath='{range .items[*]}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' | awk '{s += $1} END {print s + 0}')"
USE_VLLM=""
if [[ "$ENGINE" == "vllm" || ( "$ENGINE" == "auto" && "$GPU_COUNT" -gt 0 ) ]]; then USE_VLLM=1; fi

if [[ -z "$USE_VLLM" ]]; then
  read -r FREE_CPU FREE_MEM < <(free_room)
  NEED_CPU=$(to_milli "$CPUS"); NEED_MEM=$(to_mib "$MEMORY")
  if (( FREE_CPU < NEED_CPU || FREE_MEM < NEED_MEM )); then
    warn "the cluster has about ${FREE_CPU}m CPU and ${FREE_MEM}Mi memory free; one model copy reserves ${NEED_CPU}m and ${NEED_MEM}Mi: it may stay Pending"
  else
    ok "room for one model copy: ${FREE_CPU}m CPU and ${FREE_MEM}Mi memory free"
  fi
fi

log "Installing KubeAI ${CHART_VERSION} in namespace ${AI_NAMESPACE}"
# --repo instead of `helm repo add`: nothing shared is written, so several
# clusters can be set up at once from the same machine.
helm upgrade --install kubeai kubeai --repo https://www.kubeai.org \
  -n "$AI_NAMESPACE" --create-namespace --version "$CHART_VERSION" \
  --set open-webui.enabled=false \
  --set "modelServers.OLlama.images.default=${OLLAMA_IMAGE}" \
  --set resourceProfiles.fa-cpu.imageName=cpu \
  --set-string "resourceProfiles.fa-cpu.requests.cpu=${CPUS}" \
  --set-string "resourceProfiles.fa-cpu.requests.memory=${MEMORY}" \
  --set resourceProfiles.fa-gpu.imageName=nvidia-gpu \
  --set-string 'resourceProfiles.fa-gpu.requests.nvidia\.com/gpu=1' \
  --set-string 'resourceProfiles.fa-gpu.limits.nvidia\.com/gpu=1' \
  --wait --timeout 10m >/dev/null

if [[ -n "$USE_VLLM" ]]; then
  URL="hf://${HF_MODEL}"; ENGINE_NAME="VLLM"; PROFILE="fa-gpu:${GPUS}"
  log "Model ${NAME}: ${HF_MODEL} on vLLM, ${GPUS} GPU per copy, ${MIN_REPLICAS} to ${MAX_REPLICAS} copies"
else
  URL="ollama://${MODEL}"; ENGINE_NAME="OLlama"; PROFILE="fa-cpu:1"
  log "Model ${NAME}: ${MODEL} on Ollama, ${CPUS} CPU and ${MEMORY} per copy, ${MIN_REPLICAS} to ${MAX_REPLICAS} copies"
fi
kubectl apply -f - >/dev/null <<EOF
apiVersion: kubeai.org/v1
kind: Model
metadata:
  name: ${NAME}
  namespace: ${AI_NAMESPACE}
spec:
  features: [TextGeneration]
  url: ${URL}
  engine: ${ENGINE_NAME}
  resourceProfile: ${PROFILE}
  minReplicas: ${MIN_REPLICAS}
  maxReplicas: ${MAX_REPLICAS}
EOF

if [[ -n "$WAIT" && "$MIN_REPLICAS" -gt 0 ]]; then
  log "Waiting for the model to answer (it downloads first)"
  kubectl -n "$AI_NAMESPACE" wait --for=jsonpath='{.status.replicas.ready}'="$MIN_REPLICAS" \
    "model/${NAME}" --timeout=15m >/dev/null
  ok "model ${NAME} ready"
fi

echo "AI_URL=http://kubeai.${AI_NAMESPACE}.svc/openai/v1"
echo "AI_MODEL=${NAME}"
