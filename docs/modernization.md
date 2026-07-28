# Modernization status

KubeZoo was restarted after several years of limited maintenance. This file
records completed infrastructure work and the remaining compatibility boundary.

## Current toolchain

- Go module baseline: 1.24
- Preferred Go toolchain: 1.26.5
- Container base: Alpine Linux 3.24
- Published platforms: linux/amd64 and linux/arm64

## Completed infrastructure work

- Replaced ad-hoc builds and unpinned tool installation with repository-owned
  targets and pinned tooling.
- Added build, unit/integration test, lint, race, vulnerability, CodeQL, and
  dependency-review workflows.
- Added Dependabot for Go, Actions, and Docker.
- Added non-root images, BuildKit caches, SBOMs, provenance, and attestations.
- Separated unit tests from envtest integration tests.

## Kubernetes dependency boundary

The module declares a mixture of Kubernetes 1.24 and 1.25 requirements, then
uses `replace` directives to force the Kubernetes dependency family onto the
1.24 implementation. KubeZoo also imports internal `k8s.io/kubernetes`
packages and depends on `sigs.k8s.io/apiserver-runtime` and legacy generated
clients. A mechanical version bump is therefore unsafe.

The core migration requires:

1. Select one maintained Kubernetes minor and align every `k8s.io/*` module.
2. Remove imports of internal `k8s.io/kubernetes` packages where supported
   public APIs exist.
3. Upgrade controller-runtime, apiserver-runtime, Ginkgo, and generated clients
   as one compatibility change.
4. Regenerate protobuf, OpenAPI, CRDs, clients, listers, and informers.
5. Run request-rewrite, discovery, tenant lifecycle, quota, and envtest suites.
6. Make `govulncheck` blocking after legacy reachable findings are removed.

Until then, vulnerability reporting remains visible but non-blocking.
