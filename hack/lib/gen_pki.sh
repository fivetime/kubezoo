#!/usr/bin/env bash

# Copyright 2022 The KubeZoo Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -xue

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE}")/../.." && pwd -P)"
CONTEXT_NAME=$(kubectl config current-context)
if [ ${CONTEXT_NAME::5} != "kind-" ]; then
    echo "Current kubectl context is not a kind cluster" >&2
    exit 1
fi
KIND_SERVER="$(yq eval '.clusters.[]|select(.name=="'${CONTEXT_NAME}'")|.cluster.server' ~/.kube/config)"
readonly TEMP_DIR="${REPO_ROOT}/_output/pki"
readonly UPSTREAM_DIR=$TEMP_DIR/upstream
readonly KUBEZOO_DIR=$TEMP_DIR/kubezoo

get_upstream_pki_kind() {
    [ -z $TEMP_DIR ] || mkdir -p $TEMP_DIR

    local kind_cluster_name=${CONTEXT_NAME:5}
    local kind_docker=$(docker ps --filter "name=${kind_cluster_name}-control-plane" --format "{{.ID}}")

    [ -z $UPSTREAM_DIR ] || mkdir -p $UPSTREAM_DIR
    docker cp $kind_docker:/etc/kubernetes/pki/sa.pub $UPSTREAM_DIR/sa.pub
    docker cp $kind_docker:/etc/kubernetes/pki/sa.key $UPSTREAM_DIR/sa.key
    docker cp $kind_docker:/etc/kubernetes/pki/apiserver.crt $UPSTREAM_DIR/apiserver.crt
    docker cp $kind_docker:/etc/kubernetes/pki/apiserver.key $UPSTREAM_DIR/apiserver.key
    yq eval '.users.[]|select(.name=="'${CONTEXT_NAME}'")|.user.client-certificate-data' ~/.kube/config | base64 \
        --decode >$UPSTREAM_DIR/client.crt
    yq eval '.users.[]|select(.name=="'${CONTEXT_NAME}'")|.user.client-key-data' ~/.kube/config | base64 \
        --decode >$UPSTREAM_DIR/client-key.crt
    yq eval '.clusters.[]|select(.name=="'${CONTEXT_NAME}'")|.cluster.certificate-authority-data' \
        ~/.kube/config | base64 --decode >$UPSTREAM_DIR/ca.crt
}

gen_kubezoo_pki() {
    [ -z $KUBEZOO_DIR ] || mkdir -p $KUBEZOO_DIR
    gen_ca
    gen_admin_cert
    gen_kubernetes_cert
}

gen_ca() {
    cat >$KUBEZOO_DIR/ca-config.json <<EOF
{
  "signing": {
    "default": {
      "expiry": "8760h"
    },
    "profiles": {
      "kubernetes": {
        "usages": ["signing", "key encipherment", "server auth", "client auth"],
        "expiry": "8760h"
      }
    }
  }
}
EOF

    cat >$KUBEZOO_DIR/ca-csr.json <<EOF
{
  "CN": "Kubernetes",
  "key": {
    "algo": "rsa",
    "size": 2048
  },
  "names": [
    {
      "C": "US",
      "L": "Sunnyvale",
      "O": "KubeZoo",
      "OU": "CA",
      "ST": "CA"
    }
  ]
}
EOF
    cd $KUBEZOO_DIR
    cfssl gencert -initca $KUBEZOO_DIR/ca-csr.json | cfssljson -bare ca
    cd -
}

gen_kubernetes_cert() {

    # ⭐ kubezoo* is here because the controller reaches this server by Service
    # name. It did not used to: the controller ran inside this process and never
    # opened a connection. Splitting it out created a client that verifies this
    # certificate against a host the certificate did not name, and the failure is
    # the worst-shaped one available -- the controller starts, blocks, looks
    # healthy, and reconciles nothing, which is exactly what both manifests warn
    # happens when you forget to deploy it at all.
    #
    # ⚠️ The lab could never have caught this. It runs the controller as a host
    # process against 127.0.0.1, which was in the list already; only the manifest
    # path uses the Service name.
    KUBERNETES_HOSTNAMES=kubernetes,kubernetes.default,kubernetes.default.svc,kubernetes.default.svc.cluster,kubernetes.svc.cluster.local,localhost,host.minikube.internal,kubezoo,kubezoo.default,kubezoo.default.svc,kubezoo.default.svc.cluster.local

    cat >$KUBEZOO_DIR/kubernetes-csr.json <<EOF
{
  "CN": "kubernetes",
  "key": {
    "algo": "rsa",
    "size": 2048
  },
  "names": [
    {
      "C": "US",
      "L": "Sunnyvale",
      "O": "KubeZoo",
      "OU": "KubeZoo",
      "ST": "CA"
    }
  ]
}
EOF
    cd $KUBEZOO_DIR
    cfssl gencert \
        -ca=$KUBEZOO_DIR/ca.pem \
        -ca-key=$KUBEZOO_DIR/ca-key.pem \
        -config=$KUBEZOO_DIR/ca-config.json \
        -hostname=127.0.0.1,${KUBERNETES_HOSTNAMES} \
        -profile=kubernetes \
        $KUBEZOO_DIR/kubernetes-csr.json | cfssljson -bare kubernetes
    cd -
}

gen_admin_cert() {
    cat >$KUBEZOO_DIR/admin-csr.json <<EOF
{
  "CN": "admin",
  "key": {
    "algo": "rsa",
    "size": 2048
  },
  "names": [
    {
      "C": "US",
      "L": "Sunnyvale",
      "O": "system:masters",
      "OU": "KubeZoo",
      "ST": "CA"
    }
  ]
}
EOF
    cd $KUBEZOO_DIR
    cfssl gencert \
        -ca=$KUBEZOO_DIR/ca.pem \
        -ca-key=$KUBEZOO_DIR/ca-key.pem \
        -config=$KUBEZOO_DIR/ca-config.json \
        -profile=kubernetes \
        $KUBEZOO_DIR/admin-csr.json | cfssljson -bare admin
    cd -
}

# base64file emits a file as one unwrapped base64 line.
#
# The wrapping flag differs between implementations, so try each rather than
# asking who wrote it: the GNU test used to be a grep for "GNU" in --help, which
# the Rust coreutils base64 fails while still taking -w 0, so it fell through to
# the BSD branch and its -b was rejected. Nothing checked, so caBundle came out
# empty and the webhook it configures failed its TLS handshake later, somewhere
# else.
base64file() {
    input=${1}
    if base64 -w 0 "${input}" 2>/dev/null; then
        return 0
    fi
    if base64 -b 0 -i "${input}" 2>/dev/null; then
        return 0
    fi
    echo "base64file: no known way to emit ${input} unwrapped; tried -w 0 and -b 0 -i" >&2
    return 1
}

# gen_pod_facing_cert signs a SECOND serving certificate for kubezoo, with the
# UPSTREAM cluster's CA.
#
# ⛔ WITHOUT IT NO TENANT WORKLOAD CAN REACH KUBEZOO AT ALL, and the lab looked
# fine anyway for as long as nothing tried. A tenant Pod validates the API server
# with /var/run/secrets/kubernetes.io/serviceaccount/ca.crt, which is the
# UPSTREAM cluster's CA -- kubezoo's own CA means nothing to it. The default
# serving certificate above is signed by kubezoo's CA, which is right for the
# kubeconfigs handed to people, and useless to a Pod.
#
# ⭐ Both, by SNI, rather than replacing one with the other: the host's kubectl
# keeps trusting kubezoo's CA, and a Pod reaching the address below gets a
# certificate its own CA bundle can verify.
#
# ⚠️ tenant-api-endpoint.yaml states this prerequisite in its own comments, and
# the lab did not honour it. It surfaced only when a real operator was installed
# -- every earlier assertion about ServiceAccounts spoke to kubezoo FROM THE HOST,
# which is a different CA and a different name resolution than the path a tenant
# workload actually takes.
gen_pod_facing_cert() {
    local gateway
    gateway=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}' 2>/dev/null \
        | tr ' ' '\n' | grep -E '^[0-9]+\.' | head -1)
    if [ -z "$gateway" ]; then
        echo "gen_pod_facing_cert: could not find the kind network gateway" >&2
        return 1
    fi
    local up=$KUBEZOO_DIR/upstream-ca
    mkdir -p "$up"
    docker cp kz-audit3-control-plane:/etc/kubernetes/pki/ca.crt "$up/ca.crt" >/dev/null 2>&1
    docker cp kz-audit3-control-plane:/etc/kubernetes/pki/ca.key "$up/ca.key" >/dev/null 2>&1
    if [ ! -s "$up/ca.crt" ] || [ ! -s "$up/ca.key" ]; then
        echo "gen_pod_facing_cert: could not copy the upstream CA out of the kind node" >&2
        return 1
    fi
    # ⚠️ The SAN is a NAME, not the gateway IP, and that is not a preference.
    # TLS SNI carries a hostname; a client connecting to a bare IP sends no SNI
    # at all, so --tls-sni-cert-key keyed on an IP can never match and kubezoo
    # falls back to its default certificate -- which a Pod's CA bundle cannot
    # verify. Measured: "certificate is valid for 127.0.0.1, not 172.18.0.1".
    #
    # ⭐ Which is also the production shape: a deployment reaches kubezoo through
    # a Service name, as tenant-api-endpoint.yaml already says. The lab now does
    # the same, with a Service whose endpoints point back at the host.
    openssl req -new -newkey rsa:2048 -nodes \
        -keyout "$KUBEZOO_DIR/pod-facing-key.pem" -out "$up/pod-facing.csr" \
        -subj "/CN=kubezoo" >/dev/null 2>&1
    openssl x509 -req -in "$up/pod-facing.csr" \
        -CA "$up/ca.crt" -CAkey "$up/ca.key" -CAcreateserial \
        -out "$KUBEZOO_DIR/pod-facing.pem" -days 3650 \
        -extfile <(printf "subjectAltName=DNS:kubezoo.default.svc,DNS:kubezoo.default.svc.cluster.local,DNS:kubezoo.default,IP:%s,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n" "$gateway") \
        >/dev/null 2>&1
    echo "pod-facing cert for kubezoo.default.svc (via $gateway), signed by the upstream CA"
}

gen_quota_webhook_cert() {

    QUOTA_WEBHOOK_HOSTNAMES=kubezoo-cluster-resource-quota.default,kubezoo-cluster-resource-quota.default.svc

    cat >$KUBEZOO_DIR/quota-csr.json <<EOF
{
  "CN": "kubezoo-cluster-resource-quota",
  "key": {
    "algo": "rsa",
    "size": 2048
  },
  "names": [
    {
      "C": "US",
      "L": "Sunnyvale",
      "O": "system:masters",
      "OU": "KubeZoo",
      "ST": "CA"
    }
  ]
}
EOF

    cd $KUBEZOO_DIR
    cfssl gencert \
        -ca=$KUBEZOO_DIR/ca.pem \
        -ca-key=$KUBEZOO_DIR/ca-key.pem \
        -config=$KUBEZOO_DIR/ca-config.json \
        -hostname=127.0.0.1,${QUOTA_WEBHOOK_HOSTNAMES} \
        -profile=kubernetes \
        $KUBEZOO_DIR/quota-csr.json | cfssljson -bare quota-webhook

    cd -

    caBase64="$(base64file ${KUBEZOO_DIR}/ca.pem)"
    mkdir -p _output/setup
    sed "s/{caBundle}/${caBase64}/g" config/setup/quota.tmpl.yaml >_output/setup/quota.yaml
}

create_pki_secret() {
    if kubectl get secret kubezoo-pki; then
        kubectl delete secret kubezoo-pki
    fi
    # ⭐ No ca-key.pem here. The gateway verifies tenant certificates against
    # ca.pem and signs nothing; issuing them moved to kubezoo-controller. Handing
    # the signing key to a process with no use for it is the root of the tenant
    # trust chain sitting in one more place than it has to.
    kubectl create secret generic kubezoo-pki \
        --from-file=ca.pem=$KUBEZOO_DIR/ca.pem \
        --from-file=kubernetes-key.pem=$KUBEZOO_DIR/kubernetes-key.pem \
        --from-file=kubernetes.pem=$KUBEZOO_DIR/kubernetes.pem

    # The signing half, for kubezoo-controller alone.
    if kubectl get secret kubezoo-ca; then
        kubectl delete secret kubezoo-ca
    fi
    kubectl create secret generic kubezoo-ca \
        --from-file=ca.pem=$KUBEZOO_DIR/ca.pem \
        --from-file=ca-key.pem=$KUBEZOO_DIR/ca-key.pem

    if kubectl get secret upstream-pki; then
        kubectl delete secret upstream-pki
    fi
    kubectl create secret generic upstream-pki \
        --from-file=sa.pub=$UPSTREAM_DIR/sa.pub \
        --from-file=sa.key=$UPSTREAM_DIR/sa.key \
        --from-file=client.crt=$UPSTREAM_DIR/client.crt \
        --from-file=client-key.crt=$UPSTREAM_DIR/client-key.crt \
        --from-file=ca.crt=$UPSTREAM_DIR/ca.crt

    # The controller reads Tenant objects from kubezoo, so it needs a client
    # credential for kubezoo -- not for the upstream cluster, which it reaches
    # with its ServiceAccount. It runs in a different pod than kubezoo, so this
    # has to be a mounted kubeconfig rather than the loopback config the
    # in-process controller used to get for free.
    #
    # The server name is the Service, because this one is used from inside the
    # cluster. The address written into each tenant's own kubeconfig is a
    # separate setting on the controller, and is the one tenants reach from
    # outside.
    local ctrl_kubeconfig=$KUBEZOO_DIR/controller.kubeconfig
    rm -f $ctrl_kubeconfig
    kubectl --kubeconfig=$ctrl_kubeconfig config set-cluster zoo \
        --certificate-authority=$KUBEZOO_DIR/ca.pem --embed-certs=true \
        --server=https://kubezoo:6443
    kubectl --kubeconfig=$ctrl_kubeconfig config set-credentials zoo-admin \
        --client-certificate=$KUBEZOO_DIR/admin.pem \
        --client-key=$KUBEZOO_DIR/admin-key.pem --embed-certs=true
    kubectl --kubeconfig=$ctrl_kubeconfig config set-context zoo \
        --cluster=zoo --user=zoo-admin
    kubectl --kubeconfig=$ctrl_kubeconfig config use-context zoo
    if kubectl get secret kubezoo-controller-kubeconfig; then
        kubectl delete secret kubezoo-controller-kubeconfig
    fi
    kubectl create secret generic kubezoo-controller-kubeconfig \
        --from-file=kubezoo.kubeconfig=$ctrl_kubeconfig

    if kubectl get secret quota-webhook-pki; then
        kubectl delete secret quota-webhook-pki
    fi
    kubectl create secret tls quota-webhook-pki \
        --key=$KUBEZOO_DIR/quota-webhook-key.pem \
        --cert=$KUBEZOO_DIR/quota-webhook.pem
}

set_context() {
    kubectl config set-cluster zoo \
        --certificate-authority=$KUBEZOO_DIR/ca.pem \
        --embed-certs=true \
        --server=https://127.0.0.1:6443
    kubectl config set-credentials zoo-admin \
        --client-certificate=$KUBEZOO_DIR/admin.pem \
        --client-key=$KUBEZOO_DIR/admin-key.pem \
        --embed-certs=true
    kubectl config set-context zoo \
        --cluster=zoo \
        --user=zoo-admin
}

# ⚠️ Kept in step with cmd/kubezoo/app/options by hand -- nothing checks it. It is
# printed for an operator to paste, so a flag that lives here and not in the
# binary is a copy-paste that fails to start. --client-ca-key-file,
# --proxy-bind-address and --proxy-secure-port were exactly that after the
# controller moved out and stopped needing them.
kubezoo_parametes="

--allow-privileged=true \
--apiserver-count=1 \
--cors-allowed-origins=.* \
--delete-collection-workers=1 \
--etcd-prefix=/zoo \
--etcd-servers=http://localhost:2379 \
--event-ttl=1h0m0s \
--max-requests-inflight=1002 \
--service-cluster-ip-range=192.168.0.1/16 \
--service-node-port-range=20000-32767 \
--storage-backend=etcd3 \
--authorization-mode=AlwaysAllow \
--client-ca-file=$KUBEZOO_DIR/ca.pem \
--tls-cert-file=$KUBEZOO_DIR/kubernetes.pem \
--tls-private-key-file=$KUBEZOO_DIR/kubernetes-key.pem \
--service-account-key-file=$UPSTREAM_DIR/sa.pub \
--service-account-issuer=foo \
--service-account-signing-key-file=$UPSTREAM_DIR/sa.key \
--proxy-client-cert-file=$UPSTREAM_DIR/client.crt \
--proxy-client-key-file=$UPSTREAM_DIR/client-key.crt \
--proxy-client-ca-file=$UPSTREAM_DIR/ca.crt \
--request-timeout=10m \
--watch-cache=true \
--proxy-upstream-master=$KIND_SERVER \
# 上游默认 true。只管旧式 Secret 型 token:bound token 另走一个认证器,永远查存续。
# 关掉它省不下 pod 路径的开销,只会让手工建的 service-account-token 在删掉 SA 后
# 仍然有效且永不过期 —— 见 config/setup/proxy.yaml 里的完整说明。
--service-account-lookup=true \
--api-audiences=foo
"

print_kubezoo_parameters() {
    echo "$kubezoo_parametes"
}

gen_pki_setup_ctx() {
    get_upstream_pki_kind
    gen_kubezoo_pki
    gen_pod_facing_cert
    gen_quota_webhook_cert
    create_pki_secret
    set_context
}

gen_pki_setup_ctx_print_parameters() {
    get_upstream_pki_kind
    gen_kubezoo_pki
    set_context
    print_kubezoo_parameters
}

"$@"
