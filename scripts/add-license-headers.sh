#!/usr/bin/env bash
# Apply SPDX license headers to Go and TS files.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

apache=(
  "cli/kubehero"
  "services/collector"
  "packages/proto"
  "packages/cost-model"
)

bsl=(
  "services/control-plane"
  "services/operator"
  "services/pricing-engine"
  "apps/dashboard"
)

apply() {
  local dir="$1"; local spdx="$2"
  [ -d "$ROOT/$dir" ] || return 0
  find "$ROOT/$dir" \
    \( -name "*.go" -o -name "*.ts" -o -name "*.tsx" \) \
    -not -path "*/gen/*" -not -path "*/node_modules/*" -not -path "*/.next/*" \
    -print0 | while IFS= read -r -d '' f; do
    if ! head -1 "$f" | grep -q "SPDX-License-Identifier"; then
      tmp="$(mktemp)"
      { echo "// SPDX-License-Identifier: $spdx"; echo "// Copyright (c) KubeHero contributors"; echo; cat "$f"; } > "$tmp"
      mv "$tmp" "$f"
    fi
  done
}

for d in "${apache[@]}"; do apply "$d" "Apache-2.0"; done
for d in "${bsl[@]}";   do apply "$d" "BUSL-1.1";   done

echo "license headers applied"
