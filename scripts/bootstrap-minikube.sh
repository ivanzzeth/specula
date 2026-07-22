#!/usr/bin/env bash
# China / air-gapped self-bootstrap smoke on minikube + containerd.
#
# Phase 0: land Specula image (host build + minikube image load)
# Phase 1: helm install bootstrap + DaemonSet writes certs.d
# Phase 2: prefetch Job warms a small image through Specula
# Phase 3: documented only (manual helm upgrade to HA chart)
#
# Requires: minikube, helm, kubectl, docker
#
# Usage: scripts/bootstrap-minikube.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="${ROOT}/deploy/helm/specula-bootstrap"
RELEASE="${SPECULA_BOOT_RELEASE:-boot}"
NAMESPACE="${SPECULA_BOOT_NAMESPACE:-specula-boot}"
MINIKUBE_PROFILE="${MINIKUBE_PROFILE:-specula-boot}"
IMAGE_REPO="${SPECULA_IMAGE_REPO:-specula}"
IMAGE_TAG="${SPECULA_IMAGE_TAG:-ha-local}"
PREFETCH_IMAGE="${SPECULA_PREFETCH_IMAGE:-docker.io/library/hello-world:latest}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "bootstrap-minikube: required command not found: $1" >&2
    exit 1
  }
}

need minikube
need helm
need kubectl
need docker
need curl

echo "==> ensuring minikube profile '${MINIKUBE_PROFILE}' (containerd) is running"
# Host proxies that bind only to 127.0.0.1 are remapped to the docker bridge
# inside the driver VM and fail (connection refused). Unset them for start so
# kubeadm can pull via --image-repository (China-reachable Aliyun by default).
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
  # Profile config stores ContainerRuntime under KubernetesConfig.
  RUNTIME="$(python3 - "${MINIKUBE_PROFILE}" <<'PY' 2>/dev/null || true
import json, pathlib, sys
profile = sys.argv[1]
p = pathlib.Path.home() / ".minikube/profiles" / profile / "config.json"
c = json.loads(p.read_text())
print(c.get("KubernetesConfig", {}).get("ContainerRuntime") or c.get("ContainerRuntime") or "")
PY
)"
  if [[ "${RUNTIME}" != "containerd" ]]; then
    echo "bootstrap-minikube: profile '${MINIKUBE_PROFILE}' runtime is '${RUNTIME:-unknown}' (want containerd); recreate with:" >&2
    echo "  minikube delete -p ${MINIKUBE_PROFILE}" >&2
    echo "  MINIKUBE_PROFILE=${MINIKUBE_PROFILE} $0" >&2
    exit 1
  fi
  start_minikube >/dev/null
fi

echo "==> pointing kubectl at minikube"
kubectl config use-context "${MINIKUBE_PROFILE}" >/dev/null

# kindnet (CNI) is pulled after start and often fails when the node inherits a
# host proxy that only listens on 127.0.0.1. Preload from the host when needed.
preload_kindnet() {
  local img
  img="$(kubectl -n kube-system get ds kindnet -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
  [[ -z "${img}" ]] && return 0
  if kubectl -n kube-system get pods -l k8s-app=kindnet -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null | grep -q true; then
    return 0
  fi
  echo "==> preloading CNI image ${img}"
  if ! docker image inspect "${img}" >/dev/null 2>&1; then
    HTTP_PROXY="${HOST_HTTP_PROXY:-${http_proxy:-}}" \
      HTTPS_PROXY="${HOST_HTTPS_PROXY:-${https_proxy:-}}" \
      docker pull "${img}" >/dev/null
  fi
  minikube image load "${img}" -p "${MINIKUBE_PROFILE}"
  kubectl -n kube-system delete pod -l k8s-app=kindnet --wait=false >/dev/null 2>&1 || true
  kubectl wait --for=condition=Ready node --all --timeout=120s >/dev/null
}

preload_kindnet

echo "==> building Specula binary on host, packaging image, loading into minikube"
(
  unset DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH DOCKER_API_VERSION || true
  cd "${ROOT}"
  if [[ ! -f web/dist/index.html ]]; then
    echo "    building WebUI (web/dist missing)"
    (cd web && npm ci && npm run build)
  fi
  mkdir -p bin
  CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/ivanzzeth/specula/internal/version.Version=ha-local" \
    -o bin/specula ./cmd/specula
  STAGE="$(mktemp -d)"
  trap 'rm -rf "${STAGE}"' EXIT
  cp bin/specula "${STAGE}/specula"
  cp contrib/docker/specula.yaml "${STAGE}/specula.yaml"
  # Prefer rebuilding on top of an existing local tag so we do not need a
  # reachable base registry (host docker often mirrors via Specula :7732).
  if docker image inspect "${IMAGE_REPO}:${IMAGE_TAG}" >/dev/null 2>&1; then
    BASE="${IMAGE_REPO}:${IMAGE_TAG}"
  elif docker image inspect gcr.io/distroless/static-debian12:nonroot >/dev/null 2>&1; then
    BASE="gcr.io/distroless/static-debian12:nonroot"
  else
    # scratch + host CA bundle — enough for a static Go binary to dial HTTPS upstreams.
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
  elif [[ "${BASE}" == "${IMAGE_REPO}:${IMAGE_TAG}" ]]; then
    cat > "${STAGE}/Dockerfile" <<EOF
FROM ${BASE}
COPY specula /specula
COPY specula.yaml /etc/specula/specula.yaml
EOF
  else
    cat > "${STAGE}/Dockerfile" <<EOF
FROM ${BASE}
COPY specula /specula
COPY specula.yaml /etc/specula/specula.yaml
EXPOSE 7732 7733
VOLUME ["/var/lib/specula"]
USER nonroot:nonroot
ENTRYPOINT ["/specula"]
CMD ["--config", "/etc/specula/specula.yaml"]
EOF
  fi
  echo "    docker build (base=${BASE})"
  docker build -t "${IMAGE_REPO}:${IMAGE_TAG}" "${STAGE}"
)
minikube image load "${IMAGE_REPO}:${IMAGE_TAG}" -p "${MINIKUBE_PROFILE}"

echo "==> installing / upgrading bootstrap release '${RELEASE}' in '${NAMESPACE}'"
helm upgrade --install "${RELEASE}" "${CHART}" \
  --namespace "${NAMESPACE}" \
  --create-namespace \
  --set image.repository="${IMAGE_REPO}" \
  --set image.tag="${IMAGE_TAG}" \
  --set image.pullPolicy=IfNotPresent \
  --set mirror.enabled=true \
  --set prefetch.enabled=false \
  --wait --timeout=5m

echo "==> waiting for bootstrap Specula Deployment"
kubectl -n "${NAMESPACE}" rollout status "deploy/${RELEASE}-specula-bootstrap" --timeout=180s

echo "==> waiting for mirror DaemonSet"
kubectl -n "${NAMESPACE}" rollout status "ds/${RELEASE}-specula-bootstrap-mirror" --timeout=180s

echo "==> verifying containerd hosts.toml on the node"
HOSTS_TOML="$(minikube ssh -p "${MINIKUBE_PROFILE}" -- \
  'sudo cat /etc/containerd/certs.d/docker.io/hosts.toml' 2>/dev/null || true)"
if ! echo "${HOSTS_TOML}" | grep -q '127.0.0.1:30732'; then
  echo "bootstrap-minikube: hosts.toml missing mirror endpoint:" >&2
  echo "${HOSTS_TOML}" >&2
  exit 1
fi
echo "    hosts.toml OK (points at 127.0.0.1:30732)"

echo "==> Phase 0/1 smoke: /healthz + /v2/ via NodePort"
NODE_IP="$(minikube ip -p "${MINIKUBE_PROFILE}")"
if ! curl -sfS "http://${NODE_IP}:30732/healthz" >/dev/null; then
  echo "bootstrap-minikube: /healthz failed on NodePort" >&2
  exit 1
fi
# Docker registry handshake returns 401 without a Bearer token — expected.
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "http://${NODE_IP}:30732/v2/" || true)"
if [[ "${CODE}" != "401" && "${CODE}" != "200" ]]; then
  echo "bootstrap-minikube: unexpected /v2/ status ${CODE}" >&2
  exit 1
fi
echo "    /healthz 200, /v2/ ${CODE}"

echo "==> Phase 2: enable prefetch for ${PREFETCH_IMAGE}"
helm upgrade "${RELEASE}" "${CHART}" \
  --namespace "${NAMESPACE}" \
  --reuse-values \
  --set prefetch.enabled=true \
  --set-string "prefetch.images[0]=${PREFETCH_IMAGE}" \
  --wait --timeout=5m

echo "==> waiting for prefetch Job"
JOB="${RELEASE}-specula-bootstrap-prefetch"
for _ in $(seq 1 60); do
  SUCCEEDED="$(kubectl -n "${NAMESPACE}" get job "${JOB}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo 0)"
  FAILED="$(kubectl -n "${NAMESPACE}" get job "${JOB}" -o jsonpath='{.status.failed}' 2>/dev/null || echo 0)"
  if [[ "${SUCCEEDED}" == "1" ]]; then
    break
  fi
  if [[ "${FAILED}" != "" && "${FAILED}" != "0" ]]; then
    kubectl -n "${NAMESPACE}" logs "job/${JOB}" --tail=80 >&2 || true
    echo "bootstrap-minikube: prefetch Job failed" >&2
    exit 1
  fi
  sleep 2
done
SUCCEEDED="$(kubectl -n "${NAMESPACE}" get job "${JOB}" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo 0)"
if [[ "${SUCCEEDED}" != "1" ]]; then
  kubectl -n "${NAMESPACE}" logs "job/${JOB}" --tail=80 >&2 || true
  echo "bootstrap-minikube: prefetch Job did not succeed in time" >&2
  exit 1
fi
echo "    prefetch Job succeeded"

echo "==> re-prefetch via CLI (second hit should be warm / still OK)"
kubectl -n "${NAMESPACE}" run boot-prefetch-check --rm -i --restart=Never \
  --image="${IMAGE_REPO}:${IMAGE_TAG}" \
  --image-pull-policy=IfNotPresent \
  --overrides='{"spec":{"enableServiceLinks":false}}' \
  -- \
  bootstrap-prefetch \
  --addr="http://${RELEASE}-specula-bootstrap:7732" \
  --images="${PREFETCH_IMAGE}" >/tmp/boot-prefetch-check.log 2>&1 || {
  cat /tmp/boot-prefetch-check.log >&2
  exit 1
}
cat /tmp/boot-prefetch-check.log
echo "    second prefetch OK"

cat <<EOF

==> bootstrap smoke passed (Phases 0–2)

Phase 3 (manual): promote to HA when dependency images are available through the mirror:

  helm upgrade --install specula ${ROOT}/deploy/helm/specula \\
    --namespace specula --create-namespace \\
    -f ${ROOT}/deploy/helm/specula/values-minikube.yaml \\
    --set image.repository=${IMAGE_REPO} --set image.tag=${IMAGE_TAG}

Docs: deploy/helm/specula-bootstrap/README.md
EOF
