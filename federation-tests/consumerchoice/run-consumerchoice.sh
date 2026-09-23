#!/usr/bin/env bash
# Copyright 2026 Politecnico di Torino - NetGroup.
# Licensed under the Apache License, Version 2.0.
#
# Runs one ConsumerChoice end-to-end validation. Everything -- Kind clusters,
# Broker, agents, Liqo, mocks, the Ollama container and the model -- is created
# for the run and removed afterwards (unless cleanup: false / --keep-clusters).
#
#   ./federation-tests/consumerchoice/run-consumerchoice.sh --config federation-tests/consumerchoice/configs/default.yaml
#
# Extra flags are passed through: --keep-clusters, --skip-build, --run-id <id>.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

missing=()
for tool in go docker kind kubectl; do
  command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
if ((${#missing[@]})); then
  echo "error: required tool(s) not found on PATH: ${missing[*]}" >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "error: Docker is not running (docker info failed)" >&2
  exit 1
fi

if [[ $# -eq 0 ]]; then
  set -- --config federation-tests/consumerchoice/configs/default.yaml
fi

exec go run ./federation-tests/consumerchoice/ "$@"
