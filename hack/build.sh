#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ZOO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ZOO_ROOT}/hack/lib/init.sh"
source "${ZOO_ROOT}/hack/lib/build.sh"

build_binaries "$@"
