#!/usr/bin/env bash
# CRI / k8s / k3s-path real-client gate.
#
# Pins the production failure mode (containerd 2.2 colon config_path → CRI
# bypasses hosts.toml → dials *.pkg.dev) and the fix (single-root config_path
# → crictl pulls via Specula). Also runs hermetic Go CRI tests.
#
# Needs: containerd, crictl, ctr, passwordless sudo, network for LIVE pulls.
# Force skip: SPECULA_E2E_CRI=0
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-cri-k8s.XXXXXX)}"

if [[ "${SPECULA_E2E_CRI:-}" == "0" || "${SPECULA_E2E_CRI:-}" == "false" ]]; then
  echo "SKIP: SPECULA_E2E_CRI=0"
  exit 0
fi

for bin in containerd crictl ctr sudo go; do
  command -v "$bin" >/dev/null || { echo "SKIP: $bin not installed"; exit 0; }
done
sudo -n true 2>/dev/null || { echo "SKIP: passwordless sudo unavailable"; exit 0; }

step() { echo; echo "==> $*"; }

step "unit: k3s/kubeadm wiring + colon config_path rewrite + doctor"
go -C "${REPO}" test ./internal/integrate/ -count=1 \
  -run 'K3sStyle|VanillaKubeadm|BrokenHostDir|Colon|RewriteCRI|FixOneContainerd|ConfigPath|Doctor|HostsHasPublic' 

step "integration: hermetic CRI containerd (colon bypass + single-path Specula + ctr asymmetry)"
export SPECULA_E2E_CRI=1
# Exclude Live* here — next step runs them with SPECULA_E2E_LIVE=1.
go -C "${REPO}" test -tags=integration ./test/e2e/ -count=1 -timeout 180s \
  -run 'TestCRI_(Colon|Single|Ctr|Hosts)' -v

step "integration: LIVE CRI pull pause + etcd via Specula (CN mirrors)"
export SPECULA_E2E_LIVE=1
go -C "${REPO}" test -tags=integration ./test/e2e/ -count=1 -timeout 300s \
  -run 'TestCRI_LiveK8sPauseAndEtcd' -v

step "integration: LIVE path-style k8s (crane/go-containerregistry, non-CRI)"
go -C "${REPO}" test -tags=integration ./test/e2e/ -count=1 -timeout 180s \
  -run 'TestLiveK8sPause|TestLiveHelmOCIChart|TestK8sPause|TestK8sMetrics' -v

echo
echo "PASS: cri/k8s/k3s realclient gate (${WORK})"
