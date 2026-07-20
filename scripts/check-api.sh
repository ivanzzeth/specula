#!/usr/bin/env bash
# check-api.sh — smoke-check the public library surface for Library Preview.
#
# Requires: go, optionally gorelease (golang.org/x/exp/cmd/gorelease).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

echo "== go build pkg + examples =="
go build ./pkg/...
go build ./examples/...

echo "== go test pkg =="
go test ./pkg/...

echo "== public import paths present =="
for p in \
  pkg/artifact \
  pkg/cache \
  pkg/verify \
  pkg/upstream \
  pkg/coalesce \
  pkg/specula \
  pkg/embed \
  pkg/store/blob \
  pkg/store/meta \
  pkg/store/local \
  pkg/store/sqlite \
  pkg/handler/gomod \
  pkg/handler/oci
do
  test -d "$p" || { echo "missing $p"; exit 1; }
done

if command -v gorelease >/dev/null 2>&1; then
  echo "== gorelease (informational) =="
  gorelease -base=v0.2.0 -version=v0.3.0 || true
else
  echo "== gorelease skipped (install: go install golang.org/x/exp/cmd/gorelease@latest) =="
fi

echo "OK — library surface builds; see docs/LIBRARY.md and CHANGELOG.md"
