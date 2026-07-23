#!/usr/bin/env bash
# Spin up Specula HA on minikube: build local image, install Helm chart (PG/Redis/MinIO),
# verify multi-replica HA + HPA, run a short acceptance smoke.
#
# Requires: minikube, helm, kubectl, docker
#
# Usage: scripts/ha-minikube.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="${ROOT}/deploy/helm/specula"
RELEASE="${SPECULA_RELEASE:-specula}"
NAMESPACE="${SPECULA_NAMESPACE:-specula}"
MINIKUBE_PROFILE="${MINIKUBE_PROFILE:-specula-ha}"
IMAGE_REPO="${SPECULA_IMAGE_REPO:-specula}"
IMAGE_TAG="${SPECULA_IMAGE_TAG:-ha-local}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "ha-minikube: required command not found: $1" >&2
    exit 1
  }
}

need minikube
need helm
need kubectl
need docker
need curl

echo "==> ensuring minikube profile '${MINIKUBE_PROFILE}' is running"
if ! minikube status -p "${MINIKUBE_PROFILE}" >/dev/null 2>&1; then
  minikube start -p "${MINIKUBE_PROFILE}" \
    --cpus="${MINIKUBE_CPUS:-4}" \
    --memory="${MINIKUBE_MEMORY:-8192}" \
    --driver="${MINIKUBE_DRIVER:-docker}"
else
  minikube start -p "${MINIKUBE_PROFILE}" >/dev/null
fi

echo "==> pointing kubectl at minikube"
kubectl config use-context "${MINIKUBE_PROFILE}" >/dev/null

echo "==> enabling metrics-server (HPA)"
minikube addons enable metrics-server -p "${MINIKUBE_PROFILE}" >/dev/null || true
# The minikube VM's docker daemon often has an unreachable proxy, so in-cluster
# pulls of registry.k8s.io images fail. Preload metrics-server from the host and
# strip the digest pin (kubelet re-pulls digest-pinned refs even when present).
MS_IMG="$(kubectl -n kube-system get deploy metrics-server \
  -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
if [[ -n "${MS_IMG}" ]]; then
  MS_TAG="${MS_IMG%@*}" # drop @sha256:... digest so the loaded tag is used
  docker pull "${MS_TAG}" >/dev/null 2>&1 || true
  minikube image load "${MS_TAG}" -p "${MINIKUBE_PROFILE}" 2>/dev/null || true
  kubectl -n kube-system set image deploy/metrics-server "metrics-server=${MS_TAG}" >/dev/null 2>&1 || true
fi
# HPA scale test below uses a minReplicas bump and does not require live CPU
# metrics, so a lagging metrics-server is non-fatal.
kubectl -n kube-system rollout status deployment/metrics-server --timeout=120s 2>/dev/null || \
  echo "    (metrics-server not ready yet — continuing; scale test uses minReplicas)"

echo "==> building Specula binary on host, packaging ha-local image, loading into minikube"
# Avoid Dockerfile go mod download (flaky behind proxies). Host module cache + static binary.
# .dockerignore excludes bin/, so stage the binary in a temp build context.
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
  cp deploy/helm/specula/Dockerfile.ha-local "${STAGE}/Dockerfile"
  # Dockerfile.ha-local expects bin/specula + contrib path; rewrite for staged context.
  cat > "${STAGE}/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static-debian12:nonroot
COPY specula /specula
COPY specula.yaml /etc/specula/specula.yaml
EXPOSE 7732 7733
VOLUME ["/var/lib/specula"]
USER nonroot:nonroot
ENTRYPOINT ["/specula"]
CMD ["--config", "/etc/specula/specula.yaml"]
EOF
  docker build -t "${IMAGE_REPO}:${IMAGE_TAG}" "${STAGE}"
)
minikube image load "${IMAGE_REPO}:${IMAGE_TAG}" -p "${MINIKUBE_PROFILE}"

echo "==> preloading Bitnami dependency images (postgres, redis) into minikube"
# The minikube VM's docker daemon often has a stale/unreachable proxy, so
# in-cluster pulls of docker.io images fail. Pull on the host (whose proxy
# works — or via a running Specula mirror) and load straight into the VM.
# values-minikube.yaml pins these to docker.io so the refs match.
for dep_img in \
  docker.io/bitnami/postgresql:latest \
  docker.io/bitnami/redis:latest ; do
  echo "    ${dep_img}"
  if ! docker image inspect "${dep_img}" >/dev/null 2>&1; then
    docker pull "${dep_img}" >/dev/null
  fi
  minikube image load "${dep_img}" -p "${MINIKUBE_PROFILE}"
done

echo "==> fetching Bitnami chart dependencies"
if [[ -d "${CHART}/charts" ]] && ls "${CHART}/charts"/*.tgz >/dev/null 2>&1; then
  echo "    charts/ already present — skipping helm dependency update"
else
  helm dependency update "${CHART}"
fi

echo "==> installing / upgrading Helm release '${RELEASE}' in namespace '${NAMESPACE}'"
helm upgrade --install "${RELEASE}" "${CHART}" \
  --namespace "${NAMESPACE}" \
  --create-namespace \
  -f "${CHART}/values-minikube.yaml" \
  --set "image.repository=${IMAGE_REPO}" \
  --set "image.tag=${IMAGE_TAG}" \
  --set "image.pullPolicy=IfNotPresent" \
  --wait \
  --timeout 20m

echo "==> waiting for Specula pods"
kubectl -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=10m
kubectl -n "${NAMESPACE}" wait --for=condition=ready pod \
  -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/name=specula" \
  --timeout=10m

echo ""
echo "==> pod / HPA status"
kubectl -n "${NAMESPACE}" get pods,hpa,deploy -l "app.kubernetes.io/instance=${RELEASE}"

echo "==> acceptance: data-plane /v2/ via port-forward"
PF_LOG="$(mktemp)"
kubectl -n "${NAMESPACE}" port-forward "svc/${RELEASE}" 17732:7732 17733:7733 >"${PF_LOG}" 2>&1 &
PF_PID=$!
cleanup() { kill "${PF_PID}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Wait for port-forward to accept connections.
for _ in $(seq 1 30); do
  if curl -sf -o /dev/null "http://127.0.0.1:17732/v2/" 2>/dev/null; then
    break
  fi
  sleep 1
done

HTTP="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:17732/v2/" || true)"
if [[ "${HTTP}" != "200" && "${HTTP}" != "401" ]]; then
  echo "ha-minikube: unexpected /v2/ status ${HTTP}" >&2
  cat "${PF_LOG}" >&2 || true
  exit 1
fi
echo "    /v2/ → HTTP ${HTTP} (ok)"

CTRL="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:17733/healthz" || true)"
if [[ "${CTRL}" != "200" ]]; then
  echo "ha-minikube: control /healthz status ${CTRL}" >&2
  exit 1
fi
echo "    control /healthz → HTTP ${CTRL} (ok)"

echo "==> acceptance: warm pull-through cache (manifest)"
WARM_PATH="/v2/library/hello-world/manifests/latest"
ACCEPT='Accept: application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.oci.image.index.v1+json'
WARM_CODE=""
for _ in $(seq 1 40); do
  WARM_CODE="$(curl -sS -o "${ROOT}/.ha-warm-body" -w '%{http_code}' --max-time 60 \
    -H "${ACCEPT}" "http://127.0.0.1:17732${WARM_PATH}" || true)"
  if [[ "${WARM_CODE}" == "200" ]]; then
    break
  fi
  sleep 2
done
if [[ "${WARM_CODE}" != "200" ]]; then
  echo "ha-minikube: warm manifest fetch failed (HTTP ${WARM_CODE}); network/upstream may be unreachable — continuing with kill-pod check only" >&2
  WARM_OK=0
else
  echo "    ${WARM_PATH} → HTTP 200 (warmed)"
  WARM_OK=1
fi

echo "==> acceptance: kill one Specula pod — others keep serving"
VICTIM="$(kubectl -n "${NAMESPACE}" get pod -l "app.kubernetes.io/name=specula,app.kubernetes.io/instance=${RELEASE}" \
  -o jsonpath='{.items[0].metadata.name}')"
kubectl -n "${NAMESPACE}" delete pod "${VICTIM}" --wait=false
sleep 2
HTTP2=""
for _ in $(seq 1 20); do
  HTTP2="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:17732/v2/" || true)"
  if [[ "${HTTP2}" == "200" || "${HTTP2}" == "401" ]]; then
    break
  fi
  sleep 0.5
done
if [[ "${HTTP2}" != "200" && "${HTTP2}" != "401" ]]; then
  echo "ha-minikube: /v2/ failed during pod kill (${HTTP2})" >&2
  exit 1
fi
echo "    after delete ${VICTIM}: /v2/ → HTTP ${HTTP2} (ok)"

if [[ "${WARM_OK}" -eq 1 ]]; then
  echo "==> acceptance: re-fetch warmed manifest after pod kill (shared CAS hit)"
  HIT_CODE=""
  for _ in $(seq 1 20); do
    HIT_CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 \
      -H "${ACCEPT}" "http://127.0.0.1:17732${WARM_PATH}" || true)"
    if [[ "${HIT_CODE}" == "200" ]]; then
      break
    fi
    sleep 0.5
  done
  if [[ "${HIT_CODE}" != "200" ]]; then
    echo "ha-minikube: warmed manifest re-fetch failed after kill (HTTP ${HIT_CODE})" >&2
    exit 1
  fi
  echo "    ${WARM_PATH} → HTTP 200 (cache hit across replicas)"
fi

kubectl -n "${NAMESPACE}" wait --for=condition=ready pod \
  -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/name=specula" \
  --timeout=5m

echo "==> acceptance: HPA present + scale-up exercise"
kubectl -n "${NAMESPACE}" get hpa "${RELEASE}"
# Force a temporary higher floor so we observe scale-up without artificial CPU load.
helm upgrade "${RELEASE}" "${CHART}" \
  --namespace "${NAMESPACE}" \
  -f "${CHART}/values-minikube.yaml" \
  --set "image.repository=${IMAGE_REPO}" \
  --set "image.tag=${IMAGE_TAG}" \
  --set "autoscaling.minReplicas=4" \
  --reuse-values \
  --wait \
  --timeout 10m
kubectl -n "${NAMESPACE}" wait --for=jsonpath='{.status.readyReplicas}'=4 \
  "deployment/${RELEASE}" --timeout=5m
READY="$(kubectl -n "${NAMESPACE}" get deploy "${RELEASE}" -o jsonpath='{.status.readyReplicas}')"
echo "    scaled to readyReplicas=${READY}"
if [[ "${READY}" -lt 4 ]]; then
  echo "ha-minikube: expected ≥4 ready replicas after HPA minReplicas bump" >&2
  exit 1
fi
# Restore demo defaults.
helm upgrade "${RELEASE}" "${CHART}" \
  --namespace "${NAMESPACE}" \
  -f "${CHART}/values-minikube.yaml" \
  --set "image.repository=${IMAGE_REPO}" \
  --set "image.tag=${IMAGE_TAG}" \
  --wait \
  --timeout 10m

echo ""
echo "==> final status"
kubectl -n "${NAMESPACE}" get pods,hpa,deploy -l "app.kubernetes.io/instance=${RELEASE}"

echo ""
echo "Done. HA + HPA smoke passed."
echo "  Data plane:    kubectl -n ${NAMESPACE} port-forward svc/${RELEASE} 7732:7732"
echo "  Control plane: kubectl -n ${NAMESPACE} port-forward svc/${RELEASE} 7733:7733"
