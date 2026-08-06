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

set -eu

ZOO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
source "${ZOO_ROOT}/hack/lib/init.sh"

# ⛔ cfssl, cfssljson and yq are VENDORED in bin/, and this script did not look
# there. hack/lab/up.sh has had this line for a long time; local_up.sh never got
# it. The symptom was not "command not found, stop": gen_pki.sh calls yq inside a
# command substitution, which `set -e` does not catch, so KIND_SERVER came back
# empty and the run HUNG. A setup step that fails quietly does not produce one
# clear failure -- it produces a hang, or several misleading ones further down.
export PATH="${ZOO_ROOT}/bin:$PATH"

readonly REQUIRED_CMD=(
    go
    docker
    kubectl
    kind
    # Needed by hack/lib/gen_pki.sh. Missing from this list until a run hung on
    # the yq call instead of reporting it.
    yq
    cfssl
    cfssljson
    openssl
)

# ⚠️ buildx is a separate check because it is a PLUGIN: `docker` can be present
# and `docker buildx` still missing, and the Dockerfiles need it -- they use
# BUILDPLATFORM/TARGETARCH, which the legacy builder rejects outright.
check_buildx() {
    docker buildx version >/dev/null 2>&1 || {
        echo "docker buildx is not installed; the Dockerfiles here need BuildKit." >&2
        echo "See https://docs.docker.com/go/buildx/" >&2
        exit 1
    }
}

# The images the manifests in config/setup reference. local_up builds them from
# THIS working copy and loads them into kind, so what comes up is the code in
# front of you.
#
# ⛔ This used to be `docker pull kubezoo/kubezoo:v0.2.0` -- the UPSTREAM
# project's image, from a fork that has diverged a long way. `make local-up`
# brought up somebody else's kubezoo and none of this repository's code.
readonly IMAGE_REPO="${IMAGE_REPO:-ghcr.io/fivetime}"
readonly LOCAL_UP_IMAGE_TAG="${LOCAL_UP_IMAGE_TAG:-latest}"
readonly LOCAL_ARCH=$(go env GOHOSTARCH)
readonly LOCAL_OS=$(go env GOHOSTOS)
readonly CLUSTER_NAME="kubezoo-e2e-test"
readonly KIND_KUBECONFIG=${KIND_KUBECONFIG:-${HOME}/.kube/config}

cleanup() {
    rm -rf $ZOO_ROOT/_output
    if kind get clusters | grep "${CLUSTER_NAME}"; then
        kubectl --context "kind-${CLUSTER_NAME}" delete statefulset --all
        kubectl --context "kind-${CLUSTER_NAME}" delete deployment --all
        kubectl --context "kind-${CLUSTER_NAME}" delete validatingwebhookconfigurations --all
    fi
}

cleanup_on_err() {
    if [[ $? -ne 0 ]]; then
        cleanup
    fi
}

preflight() {
    echo "Preflight Check..."
    local missing=()
    for bin in "${REQUIRED_CMD[@]}"; do
        command -v "${bin}" >/dev/null 2>&1 || missing+=("${bin}")
    done
    # ⚠️ This was `|| (echo ... && exit 0)`: two bugs in one line. The exit ran in
    # a SUBSHELL, so the script carried on anyway, and its status was 0 -- a run
    # missing its tools reported success and then failed somewhere unrelated.
    if [ ${#missing[@]} -ne 0 ]; then
        echo "missing required commands: ${missing[*]}" >&2
        echo "(cfssl, cfssljson and yq are vendored in ${ZOO_ROOT}/bin)" >&2
        exit 1
    fi
}

local_up() {
    CONTROLLER_ROOT=${KUBEZOO_CONTROLLER_DIR:-$ZOO_ROOT/../kubezoo-controller}
    echo "Creating the kind cluster $CLUSTER_NAME..."
    if kind get clusters | grep "${CLUSTER_NAME}"; then
        cleanup
    else
        kind create cluster --name "${CLUSTER_NAME}"
    fi
    kubectl config use-context "kind-${CLUSTER_NAME}"

    echo "Generating PKI and context..."
    bash "${ZOO_ROOT}"/hack/lib/gen_pki.sh gen_pki_setup_ctx

    # Built from source, then loaded into the kind node. The manifests in
    # config/setup name the images CI publishes; loading a locally built image
    # under the same name is what makes `imagePullPolicy: IfNotPresent` prefer it.
    echo "Building images from this working copy..."
    make -C "${ZOO_ROOT}" docker-build \
        IMAGE_REPO="${IMAGE_REPO}" IMAGE_TAG="${LOCAL_UP_IMAGE_TAG}" \
        TARGET_PLATFORMS="linux/${LOCAL_ARCH}"

    local images=(
        "${IMAGE_REPO}/kubezoo:${LOCAL_UP_IMAGE_TAG}"
        "${IMAGE_REPO}/kubezoo-clusterresourcequota:${LOCAL_UP_IMAGE_TAG}"
    )
    # The controller lives in its own repository since the split, and it is not
    # optional: without it tenants are accepted and never reconciled.
    if [ -f "${CONTROLLER_ROOT}/build/kubezoo-controller.Dockerfile" ]; then
        echo "Building kubezoo-controller from ${CONTROLLER_ROOT}..."
        make -C "${CONTROLLER_ROOT}" docker-build \
            IMAGE_REPO="${IMAGE_REPO}" IMAGE_TAG="${LOCAL_UP_IMAGE_TAG}" \
            TARGET_PLATFORMS="linux/${LOCAL_ARCH}"
        images+=("${IMAGE_REPO}/kubezoo-controller:${LOCAL_UP_IMAGE_TAG}")
    fi

    echo "Loading images into $CLUSTER_NAME..."
    kind load docker-image --name "${CLUSTER_NAME}" "${images[@]}"

    echo "Setting up ClusterResourceQuota on $CLUSTER_NAME..."
    # ⚠️ No `kubectl apply` of the CRD here, and that is not an omission: the
    # quota controller installs it itself at startup, from the copy embedded in
    # kubezoo-contract. Applying it here as well would make the deployment a
    # second source for something that already has one.
    #
    # ⛔ The wait below is therefore waiting on the CONTROLLER, not on a manifest.
    # If it spins forever, the controller did not start or could not install the
    # CRD -- look at its logs rather than for a missing yaml.
    # run quota controller and webhook
    kubectl --context "kind-${CLUSTER_NAME}" apply -f $ZOO_ROOT/_output/setup/quota.yaml
    while ! kubectl --context "kind-${CLUSTER_NAME}" get clusterresourcequota; do
        echo ">> clusterresourcequota is not ready, sleep 1s"
        sleep 1s
    done

    echo "Setting up kubezoo on $CLUSTER_NAME..."
    # ⛔ The audit policy is a ConfigMap the StatefulSet mounts, and nothing here
    # created it: kubezoo-0 sat in ContainerCreating for ever on
    # "configmap kubezoo-audit-policy not found". Auditing was added after this
    # script was last touched, and hack/lab/up.sh -- which runs kubezoo as a
    # binary with --audit-policy-file -- never needed a ConfigMap at all, so the
    # gap only existed on this path.
    kubectl create configmap kubezoo-audit-policy \
        --from-file=policy.yaml="$ZOO_ROOT/config/setup/audit-policy.yaml" \
        --dry-run=client -o yaml | kubectl apply -f -
    kubectl apply -f $ZOO_ROOT/config/setup/proxy.yaml

    while ! (kubectl --context "kind-${CLUSTER_NAME}" get pods kubezoo-0 | grep "Running"); do
        echo ">> wait for kubezoo server running"
        sleep 1s
    done

    # The tenant controller is a separate deployment now, from a separate
    # repository. Without it the cluster accepts Tenant objects and creates
    # nothing for them, so bringing up only the proxy would look like a working
    # kubezoo right until the first tenant does not get its namespaces.
    if [ -f "$CONTROLLER_ROOT/config/setup/controller.yaml" ]; then
        echo "Setting up kubezoo-controller from $CONTROLLER_ROOT..."
        kubectl apply -f "$CONTROLLER_ROOT/config/setup/controller.yaml"
    else
        echo ">> WARNING: kubezoo-controller not found at $CONTROLLER_ROOT."
        echo ">> Tenants will be accepted and never reconciled. Set KUBEZOO_CONTROLLER_DIR"
        echo ">> or apply its config/setup/controller.yaml by hand."
    fi

    echo "Export kubezoo server to 6443"
    kubectl --context "kind-${CLUSTER_NAME}" port-forward svc/kubezoo 6443:6443

}

preflight
check_buildx
local_up
