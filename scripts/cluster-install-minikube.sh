#!/usr/bin/env bash
# CN-safe one-command cluster install gate on minikube + containerd.
#
# Builds Specula, loads the image, runs:
#   specula cluster install --cn --load-image --wait
# then: specula cluster doctor + crictl pull registry.k8s.io/pause:3.9
#
# Requires: minikube, helm, kubectl, docker, curl
# Usage: scripts/cluster-install-minikube.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MINIKUBE_PROFILE="${MINIKUBE_PROFILE:-specula-cluster}"
IMAGE_REPO="${SPECULA_IMAGE_REPO:-specula}"
IMAGE_TAG="${SPECULA_IMAGE_TAG:-local}"
NAMESPACE="${SPECULA_BOOT_NAMESPACE:-specula-boot}"
RELEASE="${SPECULA_BOOT_RELEASE:-boot}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "cluster-install-minikube: required command not found: $1" >&2
    exit 1
  }
}

need minikube
need helm
need kubectl
need docker
need curl
need go

echo "==> ensuring minikube profile '${MINIKUBE_PROFILE}' (containerd)"
start_minikube() {
  env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
    -u ALL_PROXY -u all_proxy \
    minikube start -p "${MINIKUBE_PROFILE}" "$@"
}
if ! minikube status -p "${MINIKUBE_PROFILE}" >/dev/null 2>&1; then
  start_minikube \
    --container-runtime=containerd \
    --cpus="${MINIKUBE_CPUS:-2}" \
    --memory="${MINIKUBE_MEMORY:-4096}" \
    --driver="${MINIKUBE_DRIVER:-docker}" \
    --image-repository="${MINIKUBE_IMAGE_REPO:-registry.cn-hangzhou.aliyuncs.com/google_containers}"
else
  start_minikube >/dev/null
fi

kubectl config use-context "${MINIKUBE_PROFILE}" >/dev/null

echo "==> building Specula binary + local image"
(
  unset DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH DOCKER_API_VERSION || true
  cd "${ROOT}"
  if [[ ! -f web/dist/index.html ]]; then
    (cd web && npm ci && npm run build)
  fi
  mkdir -p bin
  CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/ivanzzeth/specula/internal/version.Version=cluster-local" \
    -o bin/specula ./cmd/specula
  STAGE="$(mktemp -d)"
  trap 'rm -rf "${STAGE}"' EXIT
  cp bin/specula "${STAGE}/specula"
  cp contrib/docker/specula.yaml "${STAGE}/specula.yaml"
  if docker image inspect "${IMAGE_REPO}:${IMAGE_TAG}" >/dev/null 2>&1; then
    BASE="${IMAGE_REPO}:${IMAGE_TAG}"
  else
    BASE="scratch"
    cp /etc/ssl/certs/ca-certificates.crt "${STAGE}/ca-certificates.crt"
  fi
  if [[ "${BASE}" == "scratch" ]]; then
    cat > "${STAGE}/Dockerfile" <<'EOF'
FROM scratch
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY specula /specula
COPY specula.yaml /etc/specula/specula.yaml
EXPOSE 7732 7733
VOLUME ["/var/lib/specula"]
ENTRYPOINT ["/specula"]
CMD ["--config", "/etc/specula/specula.yaml"]
EOF
  else
    cat > "${STAGE}/Dockerfile" <<EOF
FROM ${BASE}
COPY specula /specula
COPY specula.yaml /etc/specula/specula.yaml
EOF
  fi
  docker build -t "${IMAGE_REPO}:${IMAGE_TAG}" "${STAGE}"
)

echo "==> preloading image into minikube (explicit)"
minikube image load "${IMAGE_REPO}:${IMAGE_TAG}" -p "${MINIKUBE_PROFILE}"

echo "==> cleaning previous bootstrap release (if any)"
"${ROOT}/bin/specula" cluster uninstall \
  --context "${MINIKUBE_PROFILE}" \
  --namespace "${NAMESPACE}" \
  --release "${RELEASE}" >/dev/null 2>&1 || true

echo "==> specula cluster install --cn"
"${ROOT}/bin/specula" cluster install \
  --context "${MINIKUBE_PROFILE}" \
  --chart-dir "${ROOT}/deploy/helm/specula-bootstrap" \
  --namespace "${NAMESPACE}" \
  --release "${RELEASE}" \
  --image "${IMAGE_REPO}:${IMAGE_TAG}" \
  --cn \
  --wait \
  --timeout 5m

echo "==> waiting for node Ready after integrate reload=once (if any)"
for i in $(seq 1 60); do
  if kubectl --context "${MINIKUBE_PROFILE}" get nodes >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
kubectl --context "${MINIKUBE_PROFILE}" wait --for=condition=Ready node --all --timeout=180s >/dev/null
kubectl --context "${MINIKUBE_PROFILE}" -n "${NAMESPACE}" rollout status \
  "deploy/${RELEASE}-specula-bootstrap" --timeout=180s
kubectl --context "${MINIKUBE_PROFILE}" -n "${NAMESPACE}" rollout status \
  "ds/${RELEASE}-specula-bootstrap-integrate" --timeout=120s >/dev/null 2>&1 || true

# Stamp proves once-reload path ran (or config already matched).
STAMP="$(minikube ssh -p "${MINIKUBE_PROFILE}" -- 'sudo test -f /var/lib/specula/.cri-reload-hash && echo ok' 2>/dev/null || true)"
if [[ "${STAMP}" != *ok* ]]; then
  echo "cluster-install-minikube: missing /var/lib/specula/.cri-reload-hash (once-reload stamp)" >&2
  exit 1
fi
echo "    cri-reload stamp OK"

echo "==> self-heal: restart Specula Deployment and wait for Ready"
kubectl --context "${MINIKUBE_PROFILE}" -n "${NAMESPACE}" rollout restart \
  "deploy/${RELEASE}-specula-bootstrap"
kubectl --context "${MINIKUBE_PROFILE}" -n "${NAMESPACE}" rollout status \
  "deploy/${RELEASE}-specula-bootstrap" --timeout=180s
# PVC must still be Bound after recreate.
PVC_PHASE="$(kubectl --context "${MINIKUBE_PROFILE}" -n "${NAMESPACE}" get pvc \
  "${RELEASE}-specula-bootstrap-data" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
if [[ "${PVC_PHASE}" != "Bound" ]]; then
  echo "cluster-install-minikube: expected Bound PVC after restart, got '${PVC_PHASE}'" >&2
  exit 1
fi
echo "    PVC still Bound after Deployment restart"

echo "==> specula cluster doctor"
"${ROOT}/bin/specula" cluster doctor \
  --context "${MINIKUBE_PROFILE}" \
  --namespace "${NAMESPACE}" \
  --release "${RELEASE}"

echo "==> crictl pull registry.k8s.io/pause:3.9 via Specula hosts.toml"
minikube ssh -p "${MINIKUBE_PROFILE}" -- \
  'sudo crictl pull registry.k8s.io/pause:3.9' 2>&1 | tee /tmp/specula-cluster-crictl-pause.log

HOSTS="$(minikube ssh -p "${MINIKUBE_PROFILE}" -- \
  'sudo cat /etc/containerd/certs.d/registry.k8s.io/hosts.toml' 2>/dev/null || true)"
if ! echo "${HOSTS}" | grep -q '127.0.0.1:30732'; then
  echo "cluster-install-minikube: registry.k8s.io hosts.toml missing NodePort mirror:" >&2
  echo "${HOSTS}" >&2
  exit 1
fi
echo "    hosts.toml OK"

CRI_PATH="$(minikube ssh -p "${MINIKUBE_PROFILE}" -- \
  "sudo grep -E \"config_path\\s*=\" /etc/containerd/config.toml | head -1" 2>/dev/null || true)"
if ! echo "${CRI_PATH}" | grep -q '/etc/containerd/certs.d'; then
  echo "cluster-install-minikube: CRI config_path must be /etc/containerd/certs.d on minikube, got:" >&2
  echo "${CRI_PATH}" >&2
  minikube ssh -p "${MINIKUBE_PROFILE}" -- 'sudo grep -n config_path /etc/containerd/config.toml' >&2 || true
  exit 1
fi
if echo "${CRI_PATH}" | grep -q 'rancher/k3s'; then
  echo "cluster-install-minikube: CRI config_path wrongly points at k3s stub path:" >&2
  echo "${CRI_PATH}" >&2
  exit 1
fi
echo "    CRI config_path OK (${CRI_PATH})"

# ConfigMap must advertise Huawei layout for k8s.
CFG="$(kubectl --context "${MINIKUBE_PROFILE}" -n "${NAMESPACE}" get cm "${RELEASE}-specula-bootstrap" \
  -o jsonpath='{.data.specula\.yaml}')"
echo "${CFG}" | grep -q 'huawei-ddn' || {
  echo "cluster-install-minikube: ConfigMap missing layout: huawei-ddn" >&2
  exit 1
}
echo "    CN remote_registries (huawei-ddn) OK"

cat <<EOF

==> cluster install smoke passed

  kubectl --context ${MINIKUBE_PROFILE} -n ${NAMESPACE} get pods,ds,svc
  ${ROOT}/bin/specula cluster doctor --context ${MINIKUBE_PROFILE}
  ${ROOT}/bin/specula cluster uninstall --context ${MINIKUBE_PROFILE}
EOF
