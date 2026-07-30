#!/usr/bin/env bash
# Rebuild the kubezoo audit lab from nothing.
#
# Upstream is a throwaway kind cluster; kubezoo runs as a local binary against a
# local etcd. The kubebrain-dbaas cluster is never touched.
set -euo pipefail

LAB="${LAB:-/tmp/kubezoo-lab}"
CLUSTER=kz-audit3
ZOO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
export KUBECONFIG=/root/.kube/config
export PATH=$ZOO/bin:$PATH
ETCD=/root/.local/share/kubebuilder-envtest/k8s/1.36.2-linux-amd64/etcd

mkdir -p "$LAB"

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  # PSA_DEFAULT=restricted makes the upstream cluster enforce a Pod Security
  # level on every namespace that does not say otherwise. It is off by default
  # because it constrains every pod any later test creates; set it to reproduce
  # the measurement that native PSA is not a tenant-proof control here.
  # WORKERS=n adds n worker nodes. The per-tenant node pool design needs more
  # than one node to mean anything: a single-node cluster has no taint that a
  # pod can fail to tolerate and nowhere for a cross-tenant binding to aim.
  WORKER_NODES=""
  if [ -n "${WORKERS:-}" ]; then
    WORKER_NODES="nodes:
- role: control-plane"
    for _ in $(seq "$WORKERS"); do
      WORKER_NODES="$WORKER_NODES
- role: worker"
    done
  fi
  PSA_PATCH=""
  if [ -n "${PSA_DEFAULT:-}" ]; then
    mkdir -p "$LAB/psa"
    cat >"$LAB/psa/admission.yaml" <<EOF
apiVersion: apiserver.config.k8s.io/v1
kind: AdmissionConfiguration
plugins:
- name: PodSecurity
  configuration:
    apiVersion: pod-security.admission.config.k8s.io/v1
    kind: PodSecurityConfiguration
    defaults:
      enforce: "$PSA_DEFAULT"
      enforce-version: "latest"
    exemptions:
      namespaces: [kube-system, local-path-storage, kyverno, ingress-nginx]
EOF
    if [ -n "$WORKER_NODES" ]; then
      echo "PSA_DEFAULT 与 WORKERS 不能同时用(两者都要写 nodes:)" >&2; exit 1
    fi
    PSA_PATCH="$(cat <<EOF
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      extraArgs:
      - name: admission-control-config-file
        value: /etc/kubernetes/psa/admission.yaml
      extraVolumes:
      - name: psa
        hostPath: /etc/kubernetes/psa
        mountPath: /etc/kubernetes/psa
        readOnly: true
        pathType: DirectoryOrCreate
  extraMounts:
  - hostPath: $LAB/psa
    containerPath: /etc/kubernetes/psa
EOF
)"
  fi
  cat >"$LAB/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: 127.0.0.1
  apiServerPort: 13486
$PSA_PATCH
$WORKER_NODES
EOF
  kind create cluster --name "$CLUSTER" --config "$LAB/kind.yaml" --image kindest/node:v1.36.1
fi
kind export kubeconfig --name "$CLUSTER"
kubectl config use-context "kind-$CLUSTER"

echo "== PKI =="
cd "$ZOO"
bash hack/lib/gen_pki.sh gen_pki_setup_ctx >"$LAB/pki.log" 2>&1

echo "== etcd =="
# Match this lab's own data directory, not "etcd --data-dir=". kind runs the
# upstream etcd as a host-visible process too, and it is only the order of its
# flags that keeps that looser pattern from matching and killing it.
pkill -f "data-dir=$LAB/etcd" || true
for i in $(seq 30); do
  ss -ltn 2>/dev/null | grep -q ':2380 ' || break
  sleep 1
done
rm -rf "$LAB/etcd"
mkdir -p "$LAB/etcd"
nohup "$ETCD" --data-dir="$LAB/etcd" --listen-client-urls=http://127.0.0.1:2379 \
  --advertise-client-urls=http://127.0.0.1:2379 \
  --listen-peer-urls=http://127.0.0.1:2380 --initial-advertise-peer-urls=http://127.0.0.1:2380 \
  --initial-cluster=default=http://127.0.0.1:2380 >"$LAB/etcd.log" 2>&1 &
for i in $(seq 30); do
  curl -s http://127.0.0.1:2379/health | grep -q true && break
  sleep 1
done
echo "etcd health: $(curl -s http://127.0.0.1:2379/health)"

echo "== kubezoo =="
pkill -f _output/local/bin/linux/amd64/kubezoo || true
sleep 1
PKI=$ZOO/_output/pki
nohup "$ZOO"/_output/local/bin/linux/amd64/kubezoo \
  --allow-privileged=true --apiserver-count=1 --cors-allowed-origins='.*' --delete-collection-workers=1 \
  --etcd-prefix=/zoo --etcd-servers=http://127.0.0.1:2379 --event-ttl=1h0m0s \
  --max-requests-inflight=1002 --service-cluster-ip-range=192.168.0.1/16 --service-node-port-range=20000-32767 \
  --storage-backend=etcd3 --authorization-mode=AlwaysAllow \
  --client-ca-file=$PKI/kubezoo/ca.pem --client-ca-key-file=$PKI/kubezoo/ca-key.pem \
  --tls-cert-file=$PKI/kubezoo/kubernetes.pem --tls-private-key-file=$PKI/kubezoo/kubernetes-key.pem \
  --service-account-key-file=$PKI/upstream/sa.pub --service-account-issuer=foo \
  --service-account-signing-key-file=$PKI/upstream/sa.key \
  --proxy-client-cert-file=$PKI/upstream/client.crt --proxy-client-key-file=$PKI/upstream/client-key.crt \
  --proxy-client-ca-file=$PKI/upstream/ca.crt --request-timeout=10m --watch-cache=true \
  --proxy-upstream-master=https://127.0.0.1:13486 --service-account-lookup=false \
  --proxy-bind-address=127.0.0.1 --proxy-secure-port=6443 --api-audiences=foo \
  >"$LAB/kubezoo.log" 2>&1 &

for i in $(seq 60); do
  if curl -sk --cert $PKI/kubezoo/admin.pem --key $PKI/kubezoo/admin-key.pem \
       https://127.0.0.1:6443/healthz | grep -q ok; then break; fi
  sleep 1
done
echo "kubezoo healthz: $(curl -sk --cert $PKI/kubezoo/admin.pem --key $PKI/kubezoo/admin-key.pem https://127.0.0.1:6443/healthz)"
# The policy layer is part of the tested shape, not an optional extra: without it
# a tenant can name any of the platform's runtime classes and take
# system-cluster-critical, so a lab without it is not testing the real thing.
echo "== kyverno =="
if ! kubectl --context "kind-$CLUSTER" get ns kyverno >/dev/null 2>&1; then
  helm repo add kyverno https://kyverno.github.io/kyverno/ >/dev/null 2>&1 || true
  helm repo update kyverno >/dev/null 2>&1 || true
  helm install kyverno kyverno/kyverno -n kyverno --create-namespace \
    --version "${KYVERNO_CHART_VERSION:-3.8.2}" \
    --kube-context "kind-$CLUSTER" --wait --timeout 8m >/dev/null
fi
kubectl --context "kind-$CLUSTER" apply -f "$ZOO/config/policy/" 2>&1 | grep -v '^Warning' || true
# config/policy/ holds one native ValidatingAdmissionPolicy alongside the Kyverno
# ones -- Kyverno cannot match the pods/binding subresource, see that file.
kubectl --context "kind-$CLUSTER" get validatingadmissionpolicy -o custom-columns=NAME:.metadata.name --no-headers 2>/dev/null | sed 's/^/vap: /' 
kubectl --context "kind-$CLUSTER" get clusterpolicy -o custom-columns=NAME:.metadata.name,READY:.status.conditions[0].status --no-headers

echo "lab up: upstream ctx = kind-kz-audit3, zoo admin ctx = zoo"
