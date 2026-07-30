# KubeZoo

KubeZoo is a lightweight Kubernetes API gateway that adds multi-tenancy to an
existing Kubernetes cluster. It isolates tenant views by transforming API
requests and responses while tenants share the underlying control plane and
data plane.

English | [简体中文](README.zh.md)

## Why KubeZoo

KubeZoo implements Kubernetes API as a Service (KAaaS). It is designed for
large numbers of small, short-lived tenants where running a dedicated cluster
or control plane per tenant would be too expensive operationally.

Key characteristics:

- namespace-backed tenant isolation;
- request, response, discovery, and object-reference rewriting;
- shared control-plane and data-plane resources;
- tenant lifecycle and cluster resource quota controllers.

Operators should start with the [operations runbook](docs/operations-cn.md): kubezoo on
its own is only half of the isolation, and the runbook covers what else has to be
installed, how to verify it took effect, and the checks that look green but prove nothing.

See the [design](docs/design.md), [architecture](docs/kaaas-platform-architecture-cn.md),
and [comparison](docs/deployment-and-comparison-cn.md) documents for details.

## Compatibility

The current runtime is based on Kubernetes 1.36. The whole `k8s.io/*` family is
pinned to 1.36.3 (`v0.36.3` for the staging modules) and the generated API stack
was regenerated against it.

KubeZoo still imports internal `k8s.io/kubernetes` packages, so a minor bump is
a deliberate port rather than a version edit. See
[modernization status](docs/modernization.md).

## Development

Prerequisites:

- Go 1.26.0 or newer (the `go.mod` baseline); Go 1.26.5 is the pinned toolchain;
- Docker with Buildx for container builds;
- Bash and Make for repository targets.

Common commands:

```bash
make build
make test-unit
make test-integration
make lint
make docker-build
```

Local binaries are written below `_output/local/bin/<os>/<arch>`.

## Installation

Review the [resource requirements](docs/resource-and-system-requirements.md),
then follow the [manual deployment guide](docs/manually-setup.md).

## Security and contributing

Read [SECURITY.md](SECURITY.md) before reporting a vulnerability. Development
and community expectations are documented in [CONTRIBUTING.md](CONTRIBUTING.md)
and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

KubeZoo is licensed under the [Apache License 2.0](LICENSE). Some
implementations are derived from Kubernetes and retain their original notices.
