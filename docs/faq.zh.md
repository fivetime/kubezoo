# FAQ

- KubeZoo 除不支持 DaemonSet 资源外，还有其他的限制吗？

> KubeZoo 默认支持 Pod、Deployment、Statefulset 等绝大部分资源。设计意图是限制对 Daemonset 和 Node 等集群共享资源的支持——若多个租户共享一个集群，出于安全和隔离的要求，不期望任何一个租户感知和操作节点(包括 Daemonset)。
>
> ⚠️ **实现现状与该意图并不一致，如实说明：**
>
> - **Node 已经不可见**（租户 list 为空、按名字 get 返回 NotFound、watch 无事件）。此前有三处豁免让节点对所有租户可见，已全部移除。
> - **DaemonSet 目前没有被拒绝**。`apigroups.go` 正常注册并代理它，实测租户可以创建成功，并在平台节点上真的把 Pod 跑起来。要落实"不感知节点"的意图，需要在准入策略层拒绝——见 `docs/kaaas-platform-architecture-cn.md` §7.3。
> - 租户仍可自由填写 `nodeSelector`、`tolerations`、`spec.nodeName`、`runtimeClassName`，这些都触及节点。同样需要策略层约束。

- KubeZoo 支持租户的 RBAC 吗？

> 支持，KubeZoo 通过 impersonate 机制模拟租户身份，故 RBAC API 与原生集群是一致的。

- 不同租户创建的 CRD 能共用吗？

> **租户级别的 CRD 已实现**：各租户之间完全隔离（组名按 `<租户 ID>-` 加前缀，两个租户可以创建同名 CRD 互不影响）。
>
> ⚠️ **系统级别 CRD 的共享机制尚未实现**，如实说明：设想中平台安装的 CRD（由后端集群的同一个 Controller 处理）可以配置策略开放给指定的一个或多个租户。**代码里没有这个策略**——CRD 的发现与读写都只按名字前缀过滤，所以**租户看不到、也用不了任何平台安装的 CRD**。实测：平台在上游安装 `clonesets.platform.io` 后，租户侧 `api-resources` 与 `get crd` 均为空，创建对象报 `no matches for kind`。
>
> （`pkg/util/util.go` 里有一段标着 `TODO: temporary fix for system crd` 的分支，但它只作用于 ownerReference / objectReference 的转换，不影响发现与读写，且是无条件的，并非上面说的"策略"。）

- 不同租户的 Pod 部署到相同的 Node 上，性能互相影响怎么办？

> 在公有云场景下，Pod 可能会通过一些隔离性更高的服务，如弹性容器实例等进行数据面的实现，进而保证计算、存储和网络等资源的彻底隔离。

- KubeZoo 可以采用 kubectl 命令行吗，和原生是否有区别？

> 基本没有区别。KubeZoo 提供完整的 Kubernetes API 视图，租户用 kubectl 的方式与单集群一样；KubeZoo 会为租户单独签发证书、下发 Kubeconfig，用户指定正确的 Kubeconfig 即可。
>
> ⚠️ 已知的几处差异：
>
> - **`kubectl get <资源> -A`（跨全部命名空间）目前返回 Forbidden**。租户在上游的授权是按命名空间下发的，集群范围的 LIST 不在其中。改为"逐命名空间列举再合并"是待办，需先确定分页与 resourceVersion 语义。
> - **`kubectl get nodes` 返回空**（有意为之，见上文）。
> - `kubectl auth can-i` 对**集群级**资源的回答不可靠——这与原生 Kubernetes 一致（kubectl 会带上当前命名空间并自行打印警告），命名空间级资源的回答是准确的。

- KubeZoo 和 Kubernetes 自己的多租户方案 HNC 比较有哪些优势和不足呢？

> HNC 方案实现了一种层级化的 namespace 的结构，目前还在演进当中的，尚未成为 Kubernetes 的标准 API。KubeZoo 的优势在于它可以提供标准的 Kubernetes API。换而言之，若以后标准 Kubernetes API 也支持 HNC，那 KubeZoo 的每个租户也能使用 HNC，相当于 KubeZoo 是 HNC 能力的一个超集。

- KubeZoo 的实际落地场景？

> 从私有云视角来看，很多小的业务资源量诉求小，但若为这些小业务各自独立维护一个集群，则运维和资源成本高，故在私有云具备明确的场景；在公有云场景下，绝大部分的租户资源体量小，基于 KubeZoo 构建 serverless K8S 具备高效、底成本的优势。