# kubezoo-gateway

The tenant-facing API server. It adds multi-tenancy to an existing Kubernetes
cluster by translating between two views of it: tenants see a cluster of their
own, and one control plane and data plane serve all of them underneath.

It is a gateway rather than a proxy because it does not forward — it terminates
the tenant-facing API, rewrites requests and responses in both directions, serves
resources of its own, and filters what discovery advertises.

⚠️ Not to be confused with [kubesluice](https://github.com/fivetime/kubesluice), a
different project that can sit in front of this one, nor with the upstream
[kubegateway](https://github.com/kubewharf/kubegateway) it began as.

English | [简体中文](README.zh.md)

## ⚠️ This is one of three repositories

KubeZoo used to be a single repository. It is now three, and **a deployment needs
all of them**:

| | |
|---|---|
| **kubezoo-gateway** (here) | The API server tenants talk to. Terminates the tenant-facing API and translates it into upstream calls, in both directions. |
| [kubezoo-contract](https://github.com/fivetime/kubezoo-contract) | The translation rules, the API types, and the admission policies. Both other repositories depend on it. |
| [kubezoo-controller](https://github.com/fivetime/kubezoo-controller) | Reconciles the upstream cluster against the Tenant objects this server holds. |

⛔ **Running only this one gives you a cluster that accepts Tenant objects and
creates nothing for them** — no namespaces, no RoleBindings, and no error saying
what is missing. It looks like a working install until the first tenant tries to
use it.

They are separate because an apiserver is all-active and a controller is not:
fused together, every replica of this process ran a controller, and all of them
reconciled the same tenants. Split apart, how many of each to run became two
questions instead of one — and the answers differ. See kubezoo-controller for
why its answer is currently one.

## Why KubeZoo

KubeZoo implements Kubernetes API as a Service (KAaaS). It is designed for
large numbers of small, short-lived tenants where running a dedicated cluster
or control plane per tenant would be too expensive operationally.

Key characteristics:

- namespace-backed tenant isolation;
- request, response, discovery, and object-reference rewriting;
- shared control-plane and data-plane resources.

⚠️ Rewriting is not the whole of the isolation, and for cluster-scoped resources
it is not even the backstop — RBAC cannot express a name prefix, so those rest on
the rewriting being correct. The runbook says which is which.

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
make test
make lint
make docker-build
```

Local binaries are written below `_output/local/bin/<os>/<arch>`.

⚠️ **`make test` here means less than it looks.** The two suites that need a real
apiserver left with the code they belong to: reconciliation to kubezoo-controller,
and the scope-table check to kubezoo-contract. What this repository is actually
judged on is `hack/lab`, and that needs all three checked out side by side:

```bash
bash hack/lab/up.sh          # kind cluster, etcd, this server; calls the other two
bash hack/lab/verify.sh      # 118 assertions about tenant isolation
```

`up.sh` calls kubezoo-contract's `hack/lab/policies.sh` and kubezoo-controller's
`hack/lab/up-controller.sh` rather than carrying its own copy of either — override
with `KUBEZOO_CONTRACT_DIR` and `KUBEZOO_CONTROLLER_DIR`.

⭐ `verify.sh` does not split across the three repositories, and should not: every
assertion begins by creating a tenant, which needs all three running. Isolation is
a property of the whole, not of any part.

## Installation

Review the [resource requirements](docs/resource-and-system-requirements.md),
then follow the [manual deployment guide](docs/manually-setup.md).

Two manifests, applied in order:

```bash
kubectl apply -f config/setup/proxy.yaml                       # this repository
kubectl apply -f ../kubezoo-controller/config/setup/controller.yaml
```

The policies are a third piece, from kubezoo-contract. Without them a tenant can
name any of the platform's runtime classes and escape the sandbox, so they are
part of the isolation rather than an extra.

## Security and contributing

Read [SECURITY.md](SECURITY.md) before reporting a vulnerability. Development
and community expectations are documented in [CONTRIBUTING.md](CONTRIBUTING.md)
and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

KubeZoo is licensed under the [Apache License 2.0](LICENSE). Some
implementations are derived from Kubernetes and retain their original notices.
