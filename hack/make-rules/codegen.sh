#!/usr/bin/env bash
#
# Regenerates pkg/apis/openapi -- the OpenAPI definitions for the Kubernetes APIs
# this gateway serves to tenants.
#
# ⭐ It lives here, and not in kubezoo-contract with the rest of the generated
# code, because of where it is generated FROM. Everything in contract's codegen
# is generated from contract's own API types. This one is generated from
# k8s.io/api/... -- Kubernetes' types, which contract has no relationship with --
# and only this repository imports the result.
#
# ⚠️ The earlier argument for keeping it in contract was that one script produced
# it along with everything else, and splitting a generator's outputs across
# repositories is worse than a large shared module. That conflated the script
# with the source. The script was shared; the inputs never were. Moving it takes
# 65k lines and the k8s.io/kube-aggregator dependency out of a module both other
# repositories have to consume.
#
# Usage:
#   hack/make-rules/codegen.sh            regenerate in place
#   hack/make-rules/codegen.sh --verify   regenerate into a scratch copy and fail
#                                         if the committed output differs
#
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

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

BIN_DIR="${REPO_ROOT}/bin"
MODULE="github.com/fivetime/kubezoo-gateway"
HEADER="${REPO_ROOT}/hack/boilerplate.go.txt"

VERIFY=false
[[ "${1:-}" == "--verify" ]] && VERIFY=true

# The only target. Kept as a variable so a second one could be added without
# reshaping the script.
TARGETS="${TARGETS:-openapi-served}"


# APIs whose registration and defaulting are generated.

# ---------------------------------------------------------------------------
# OpenAPI inputs for pkg/apis/openapi -- the APIs KubeZoo serves to tenants.
#
# Recovered from the schemas present in the previously unreproducible file.
#
# Deliberately omitted (~100 schemas that the old file carried):
#   k8s.io/cloud-provider/config/...          component configuration file
#   k8s.io/controller-manager/config/...      formats, not APIs KubeZoo serves
#   k8s.io/kube-controller-manager/config/...
#   k8s.io/kubelet/config/...
#   k8s.io/kube-proxy/config/...
#   k8s.io/kube-scheduler/config/...
#   k8s.io/metrics/pkg/apis/...               served by metrics-server, not KubeZoo
#
# They came in when KubeZoo copied kube-apiserver's OpenAPI target list. Nothing
# outside the generated file itself references them, and cloud-provider's config
# additionally fails generation ("not sure how to enforce default for Unsupported").
# Add a package back here if KubeZoo ever starts serving it.
# ---------------------------------------------------------------------------
openapi_served_inputs() {
  local api_groups
  # Every k8s.io/api group/version the repository has types registered for.
  api_groups="$(go list k8s.io/api/... 2>/dev/null \
    | grep -E '^k8s\.io/api/[a-z0-9]+/v[0-9a-z]+$' | sort -u | tr '\n' ',')"

  echo -n "${api_groups}"
  echo -n "k8s.io/apimachinery/pkg/apis/meta/v1,"
  echo -n "k8s.io/apimachinery/pkg/apis/meta/v1beta1,"
  echo -n "k8s.io/apimachinery/pkg/api/resource,"
  echo -n "k8s.io/apimachinery/pkg/runtime,"
  echo -n "k8s.io/apimachinery/pkg/util/intstr,"
  echo -n "k8s.io/apimachinery/pkg/version,"
  echo -n "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1,"
  echo -n "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1,"
  echo -n "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1,"
  echo -n "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1beta1,"
  echo -n "k8s.io/apiserver/pkg/apis/audit/v1,"
  echo -n "k8s.io/client-go/pkg/apis/clientauthentication/v1,"
  echo -n "k8s.io/client-go/pkg/apis/clientauthentication/v1beta1,"
  echo -n "k8s.io/kubernetes/pkg/apis/abac/v1beta1"
}

# ---------------------------------------------------------------------------
# Tooling. Versions follow go.mod, replace directives included, so a version
# bump moves the generators with the code they generate.
# ---------------------------------------------------------------------------
mod_version() {
  go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' "$1"
}

# Installs a generator at the version go.mod pins, keyed by that version.
#
# This used to skip the install whenever a binary of the right name existed. A
# dependency bump then left the old generators in place forever, which is how
# the 1.36 move ended up running 1.24-era generators against 1.36 types: they
# take a different command line, so the recipe silently stopped matching the
# toolchain it claimed to follow.
install_gen() {
  local bin="$1" pkg="$2" module="$3"
  local version stamped
  version="$(mod_version "${module}")"
  stamped="${BIN_DIR}/${bin}-${version}"
  if [[ ! -x "${stamped}" ]]; then
    echo "  installing ${bin} from ${module}@${version}"
    GOBIN="${BIN_DIR}" go install "${pkg}@${version}"
    mv "${BIN_DIR}/${bin}" "${stamped}"
  fi
  ln -sf "$(basename "${stamped}")" "${BIN_DIR}/${bin}"
}

# Where generation writes.
#
# deepcopy-gen, defaulter-gen and register-gen have no output directory of their
# own any more: they resolve each input package through the module and write
# beside its source. So there is nothing to stage, and --verify cannot diff a
# staging area. Instead it copies the module to a scratch tree, generates there,
# and compares the two trees. Regeneration just uses the repository itself.
prepare_work_tree() {
  if [[ "${VERIFY}" != true ]]; then
    WORK_ROOT="${REPO_ROOT}"
    return
  fi
  WORK_ROOT="$(mktemp -d)/kubezoo"
  mkdir -p "${WORK_ROOT}"
  tar -c --exclude=./.git --exclude=./_output --exclude=./bin -C "${REPO_ROOT}" . \
    | tar -x -C "${WORK_ROOT}"
}

# Compares the scratch tree with the repository. Anything that differs is
# generated output that the checked-in copy no longer matches -- the scratch
# tree started as a copy, so untouched files cannot differ.
compare_work_tree() {
  local out status=0
  out="$(diff -ru -x .git -x _output -x bin "${REPO_ROOT}" "${WORK_ROOT}" 2>&1)" || status=$?
  if [[ "${status}" -ne 0 ]]; then
    echo "OUT OF DATE: generated code does not match the checked-in tree" >&2
    echo "${out}" | head -60 >&2
    return 1
  fi
}

# Runs a generator, dropping the "API rule violation" chatter but keeping its
# exit status. Piping into grep and appending "|| true" -- which is what this
# used to do -- hid a hard generation failure behind a successful run, so
# --verify passed while the file it was meant to guard was never produced.
run_quiet() {
  local log status=0
  log="$(mktemp)"
  "$@" >"${log}" 2>&1 || status=$?
  grep -v 'API rule violation' "${log}" >&2 || true
  rm -f "${log}"
  return "${status}"
}

run_openapi_served() {
  install_gen openapi-gen k8s.io/kube-openapi/cmd/openapi-gen k8s.io/kube-openapi
  local inputs
  IFS=, read -r -a inputs <<< "$(openapi_served_inputs)"
  run_quiet "${BIN_DIR}/openapi-gen" \
    --go-header-file "${HEADER}" \
    --output-dir "${WORK_ROOT}/pkg/apis/openapi" \
    --output-pkg "${MODULE}/pkg/apis/openapi" \
    --output-file zz_generated.openapi.go \
    "${inputs[@]}"
}

prepare_work_tree
cd "${WORK_ROOT}"

for target in ${TARGETS}; do
  echo "==> ${target}"
  case "${target}" in
    openapi-served) run_openapi_served ;;
    *) echo "unknown target: ${target}" >&2; exit 1 ;;
  esac
done

if [[ "${VERIFY}" == true ]]; then
  compare_work_tree
  echo "generated code is up to date"
else
  echo "done; review 'git diff' before committing"
fi
