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
# kubezoo signs ServiceAccount tokens with the upstream key, so its issuer and
# audiences have to be the upstream's or it cannot validate the tokens the
# kubelet projects into tenant pods -- which is how a tenant's own workload
# reaches kubezoo and sees the tenant's view of the API rather than the upstream
# one. Read it off the upstream apiserver rather than hardcoding it.
UPSTREAM_SA_ISSUER=$(kubectl --context "kind-$CLUSTER" -n kube-system get pod \
  -l component=kube-apiserver -o jsonpath='{.items[0].spec.containers[0].command}' \
  | tr ',' '\n' | grep -o 'service-account-issuer=[^"]*' | cut -d= -f2-)
UPSTREAM_SA_ISSUER=${UPSTREAM_SA_ISSUER:-https://kubernetes.default.svc.cluster.local}
echo "upstream SA issuer: $UPSTREAM_SA_ISSUER"
# Build first. The controller's script builds its own binary and the policies come
# from a pinned module, so this was the only component the lab could silently run
# a stale build of -- and it did: a run after removing three flags started the
# previous binary, which still demanded them, and the failure surfaced as a tenant
# namespace that never appeared.
( cd "$ZOO" && make build >/dev/null )
pkill -f _output/local/bin/linux/amd64/kubezoo || true
sleep 1
PKI=$ZOO/_output/pki
# ⭐ Note the asymmetry below, and do not tidy it away: ingress classes are
# published here by --public-ingress-classes, storage classes by labelling the
# object (verify.sh does the labelling). One lab run therefore exercises BOTH
# halves of the union. Making the two consistent would silently drop coverage of
# whichever one lost -- and the flag half is what keeps an upgrade from
# un-publishing everything an operator already relies on.
nohup "$ZOO"/_output/local/bin/linux/amd64/kubezoo \
  --allow-privileged=true --apiserver-count=1 --cors-allowed-origins='.*' --delete-collection-workers=1 \
  --etcd-prefix=/zoo --etcd-servers=http://127.0.0.1:2379 --event-ttl=1h0m0s \
  --max-requests-inflight=1002 --service-cluster-ip-range=192.168.0.1/16 --service-node-port-range=20000-32767 \
  --storage-backend=etcd3 --authorization-mode=AlwaysAllow \
  --client-ca-file=$PKI/kubezoo/ca.pem \
  --tls-cert-file=$PKI/kubezoo/kubernetes.pem --tls-private-key-file=$PKI/kubezoo/kubernetes-key.pem \
  --service-account-key-file=$PKI/upstream/sa.pub --service-account-issuer=$UPSTREAM_SA_ISSUER \
  --service-account-signing-key-file=$PKI/upstream/sa.key \
  --proxy-client-cert-file=$PKI/upstream/client.crt --proxy-client-key-file=$PKI/upstream/client-key.crt \
  --proxy-client-ca-file=$PKI/upstream/ca.crt --request-timeout=10m --watch-cache=true \
  --proxy-upstream-master=https://127.0.0.1:13486 --service-account-lookup=true \
  --api-audiences=$UPSTREAM_SA_ISSUER \
  --public-ingress-classes=${PUBLIC_INGRESS_CLASSES:-nginx} \
  --public-storage-classes=${PUBLIC_STORAGE_CLASSES:-} \
  --max-namespaces-per-tenant=${MAX_NAMESPACES_PER_TENANT:-16} \
  >"$LAB/kubezoo.log" 2>&1 &

for i in $(seq 60); do
  if curl -sk --cert $PKI/kubezoo/admin.pem --key $PKI/kubezoo/admin-key.pem \
       https://127.0.0.1:6443/healthz | grep -q ok; then break; fi
  sleep 1
done
echo "kubezoo healthz: $(curl -sk --cert $PKI/kubezoo/admin.pem --key $PKI/kubezoo/admin-key.pem https://127.0.0.1:6443/healthz)"

# The tenant controller is its own binary in its own repository. kubezoo alone
# accepts Tenant objects and does nothing with them -- no namespaces, no
# RoleBindings -- so a lab without this process passes nothing.
#
# How to start it lives over there, not here: a copy would drift from the flags
# that repository actually supports.
#
# ⚠️ Unlike the policies, this cannot be pinned by version: the gateway does not
# import kubezoo-controller and must not start, so there is no module graph to
# resolve it through. A path is the only handle, and the version it lands on is
# whatever is checked out.
CTRL=${KUBEZOO_CONTROLLER_DIR:-$ZOO/../kubezoo-controller}
if [ ! -x "$CTRL/hack/lab/up-controller.sh" ]; then
  echo "FATAL: kubezoo-controller not found at $CTRL; set KUBEZOO_CONTROLLER_DIR" >&2; exit 1
fi
kubectl --kubeconfig "$LAB/ctrl-zoo.kubeconfig" config set-cluster zoo \
  --certificate-authority=$PKI/kubezoo/ca.pem --embed-certs=true --server=https://127.0.0.1:6443 >/dev/null
kubectl --kubeconfig "$LAB/ctrl-zoo.kubeconfig" config set-credentials admin \
  --client-certificate=$PKI/kubezoo/admin.pem --client-key=$PKI/kubezoo/admin-key.pem --embed-certs=true >/dev/null
kubectl --kubeconfig "$LAB/ctrl-zoo.kubeconfig" config set-context c --cluster=zoo --user=admin >/dev/null
kubectl --kubeconfig "$LAB/ctrl-zoo.kubeconfig" config use-context c >/dev/null
kubectl --context "kind-$CLUSTER" config view --minify --raw > "$LAB/ctrl-upstream.kubeconfig"
"$CTRL/hack/lab/up-controller.sh" "$LAB" \
  "$LAB/ctrl-zoo.kubeconfig" "$LAB/ctrl-upstream.kubeconfig" "$PKI/kubezoo"
# The policy layer is part of the tested shape, not an optional extra: without it
# a tenant can name any of the platform's runtime classes and take
# system-cluster-critical, so a lab without it is not testing the real thing.
# The policy layer is part of the tested shape, not an optional extra: without it
# a tenant can name any of the platform's runtime classes and take
# system-cluster-critical, so a lab without it is not testing the real thing.
#
# It lives in kubezoo-contract, because the policies re-implement the same tenant
# vocabulary the Go code uses -- see that repository's hack/lab/policies.sh.
#
# ⭐ Taken from the version go.mod pins, not from whatever is checked out next
# door. The policies and the Go code are two expressions of the same rules, and
# they only agree if they come from the same release -- a lab running v0.2.0's
# code against a working copy's policies is testing a combination that will never
# ship, and nothing would have said so.
#
# KUBEZOO_CONTRACT_DIR overrides it, which is what you want when editing both at
# once. It prints which one it used, because "am I testing the tagged policies or
# my edits" is precisely the question that goes wrong quietly.
echo "== policies =="
if [ -n "${KUBEZOO_CONTRACT_DIR:-}" ]; then
  CONTRACT=$KUBEZOO_CONTRACT_DIR
  echo "policies: $CONTRACT (KUBEZOO_CONTRACT_DIR — may differ from the pinned version)"
else
  CONTRACT=$(cd "$ZOO" && go list -m -f '{{.Dir}}' github.com/fivetime/kubezoo-contract 2>/dev/null)
  CONTRACT_VERSION=$(cd "$ZOO" && go list -m -f '{{.Version}}' github.com/fivetime/kubezoo-contract 2>/dev/null)
  echo "policies: kubezoo-contract ${CONTRACT_VERSION:-?} (pinned by go.mod)"
fi
if [ -z "$CONTRACT" ] || [ ! -f "$CONTRACT/hack/lab/policies.sh" ]; then
  echo "FATAL: could not find kubezoo-contract's policies.sh (looked in '$CONTRACT')." >&2
  echo "       Run 'go mod download' or set KUBEZOO_CONTRACT_DIR." >&2
  exit 1
fi
# bash, not exec: files in the module cache are read-only and not executable.
bash "$CONTRACT/hack/lab/policies.sh" "kind-$CLUSTER" "${TENANT_DOMAIN_SUFFIX:-apps.example.com}"

echo "lab up: upstream ctx = kind-kz-audit3, zoo admin ctx = zoo"
