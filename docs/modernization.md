# Modernization status

KubeZoo was restarted after several years of limited maintenance. This file
records completed infrastructure work and the remaining compatibility boundary.

## Current toolchain

- Go module baseline: 1.26.0 (`go.mod`)
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

The migration described below has been done. The `k8s.io/*` family is aligned on
1.36.3 (`v0.36.3` for staging modules) through `replace` directives, the
generated stack was regenerated against it, and `make verify-codegen` -- here for pkg/apis/openapi, in kubezoo-contract for everything generated from the owned types -- checks that
the checked-in output still matches.

1. ~~Select one maintained Kubernetes minor and align every `k8s.io/*` module.~~
   Done: 1.36.3.
2. Remove imports of internal `k8s.io/kubernetes` packages where supported
   public APIs exist. **Still open** -- KubeZoo depends on internal types
   (`pkg/apis/core` and friends) and on a fork of the CRD handler, so a minor
   bump remains a port rather than a version edit.
3. ~~Upgrade controller-runtime, apiserver-runtime, Ginkgo, and generated
   clients as one compatibility change.~~ Done.
4. ~~Regenerate protobuf, OpenAPI, CRDs, clients, listers, and informers.~~
   Done, with the generators pinned per Kubernetes version. Note that
   `verify-codegen` previously passed while checking nothing, because it keyed
   the installed generator on the binary name and swallowed failures with
   `|| true`; both are fixed and the check has been confirmed to fail on a
   deliberately stale tree.
5. ~~Run request-rewrite, discovery, tenant lifecycle, quota, and envtest
   suites.~~ Done.
6. Make `govulncheck` blocking after legacy reachable findings are removed.
   **Still open.**

Until then, vulnerability reporting remains visible but non-blocking.

## What green does not tell you

Recorded because the port produced it three times over: compiling, `make test`
passing, `verify-codegen` (now in kubezoo-contract) passing and the binary starting are four independent
things, and none of them implies the server can serve a request. The port was
green on all four while every request failed, on three defects a compiler cannot
see -- an unset OpenAPI v3 config, the project's own types missing from the
OpenAPI definitions, and a REST storage constructor whose error was assigned to
`_`, producing a typed-nil and a segfault. Each layer needs its own evidence.
