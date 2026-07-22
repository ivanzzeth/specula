#!/usr/bin/env bash
# Vendor Bitnami subchart .tgz into deploy/helm/specula/charts/ so
# `helm upgrade --install` works offline / behind the GFW without
# contacting docker.io OCI (bitnamicharts).
#
# Specula owns this — consumers (chorei, operators) must NOT invent
# a parallel download path. Run from repo root:
#   ./scripts/vendor-helm-deps.sh
#
# When charts/*.tgz already match Chart.yaml pins, this is a no-op.
# Do NOT run `helm dependency update/build` after vendoring — those
# re-hit charts.bitnami.com → OCI and fail offline (GFW). Helm install
# from this chart directory uses charts/*.tgz directly.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/deploy/helm/specula"
OUT="$CHART/charts"
mkdir -p "$OUT"

# Pin versions must match Chart.yaml / Chart.lock.
need=(
  "postgresql:18.8.0"
  "redis:27.0.15"
  "minio:17.0.21"
)

missing=0
for entry in "${need[@]}"; do
  name="${entry%%:*}"
  version="${entry##*:}"
  dest="$OUT/${name}-${version}.tgz"
  if [[ -f "$dest" && -s "$dest" ]]; then
    echo "ok  $dest"
    continue
  fi
  missing=1
  echo "pull oci://registry-1.docker.io/bitnamicharts/${name}:${version}"
  helm pull "oci://registry-1.docker.io/bitnamicharts/${name}" \
    --version "$version" -d "$OUT"
done

if [[ "$missing" -eq 0 ]]; then
  echo "vendored charts complete (no network). helm install uses charts/*.tgz as-is."
else
  echo "pulled missing charts → $OUT"
fi
ls -la "$OUT"
