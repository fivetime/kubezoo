# FAQ

- Does kubezoo have any other restrictions except for not supporting daemonset resources?

> Kubezoo supports most of the resources such as pod, deployment and statefulset by default. The intent is to restrict cluster-sharing resources such as daemonset and node: when multiple tenants share a cluster, no tenant is expected to sense or manipulate nodes (including daemonset), for security and isolation.
>
> ⚠️ **The implementation does not fully match that intent. Plainly:**
>
> - **Nodes are no longer visible.** A tenant's list is empty, a get by name is NotFound, and a watch is silent. Three separate exemptions used to expose every node to every tenant; all three have been removed.
> - **DaemonSet is not rejected today.** It is registered and proxied like any other resource, and a tenant can create one -- verified, and the pod really does run on a platform node. Enforcing the intent needs an admission policy; see `docs/kaaas-platform-architecture-cn.md` §7.3.
> - A tenant can still set `nodeSelector`, `tolerations`, `spec.nodeName` and `runtimeClassName`, all of which touch nodes. These need the same policy layer.

- Does kubezoo support RBAC for tenants?

> Yes, kubezoo impersonates tenant identities through the impersonate mechanism, so the RBAC API is consistent with native API of kubernetes.

- Does CRD share across the tenants?

> **Tenant CRDs are implemented** and completely isolated between tenants: the API group is prefixed with the tenant id, so two tenants can define the same CRD without collision.
>
> ⚠️ **System CRD sharing is not implemented.** The idea is that a CRD installed by the platform, reconciled by one controller in the backend cluster, could be opened to one or more designated tenants by policy. **There is no such policy in the code** -- CRD discovery and access filter on the name prefix only, so **a tenant can neither see nor use any platform-installed CRD**. Verified: with `clonesets.platform.io` installed upstream, a tenant's `api-resources` and `get crd` are both empty and creating an object fails with `no matches for kind`.
>
> (`pkg/util/util.go` carries a branch marked `TODO: temporary fix for system crd`, but it only affects ownerReference and objectReference conversion, does not reach discovery or read/write, and is unconditional rather than the policy described above.)

- What if pods of different tenants are deployed on the same Node and their performance affects each other?

> In the public cloud scenario, we could implement the data plane through some services such as elastic container instance with higher isolation to ensure the complete isolation of computing, storage, and network resources.

- Does kubezoo need a dedicated kubectl?

> No. Kubezoo serves the full Kubernetes API, so each tenant uses kubectl the same way as against a single cluster.
>
> ⚠️ Known differences:
>
> - **`kubectl get <resource> -A` is Forbidden.** A tenant's upstream grants are per namespace, and a cluster-scoped LIST is not among them. Replacing it with a per-namespace fan-out is pending, and needs the paging and resourceVersion semantics settled first.
> - **`kubectl get nodes` returns nothing**, by design (see above).
> - `kubectl auth can-i` is unreliable for **cluster-scoped** resources. This matches stock Kubernetes -- kubectl sends the current namespace and prints its own warning about it -- and the answers for namespaced resources are accurate.

- What are the advantages and disadvantages of kubezoo and kubernetes's HNC?

> The HNC solution implements a hierarchical namespace structure that is still evolving and has not yet become the standard API of kubernetes yet. The advantage of kubezoo is that it provides the standard kubernetes API. In other words, if HNC will be supported by the standard kubernetes API in the future, then every tenant of kubezoo will be able to use HNC.

- What is the landing scene of kubezoo?

> From the perspective of private cloud, many small service resources have small demands. However, if a cluster is independently maintained for these small services, the operation and resource costs are high. Therefore, private cloud has a clear scenario. In the public cloud scenario, most tenants need only a lit resources, so the construction of serverless kubernetes based on kubezoo has the advantages of high efficiency and low cost.