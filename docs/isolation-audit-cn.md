# kubezoo 隔离正确性审计(#82)

对着**真实上游集群**(kind v1.35 + kubezoo 移植版 + etcd)做的双租户黑盒审计,不是读码结论。
每条结论都注明了是**实测**还是**读码**。

审计时的代码状态:`e01e169`(per-namespace RBAC 已落地)。

## 结论摘要

| # | 问题 | 严重度 | 状态 |
|---|---|---|---|
| A | 租户可注册**全集群生效**的准入 webhook,打死其他租户与平台 | ⛔ 最高 | 实测坐实 |
| B | PersistentVolume 完全未改写:撞名 + 存在性泄露 + 对象永久滞留 | ⛔ 高 | 实测坐实 |
| C | PVC 的 `spec.volumeName` 未改写 | ⚠️ 中 | 实测坐实 |
| D | `PVTranformer` / `PVCTransformer` 写好了但**从未接线** | ⚠️ 中(A/B/C 的成因) | 实测坐实 |
| E | Node 对所有租户可见 | ⚠️ 中 | 实测确认(已知,TODO 1.2) |
| — | CRD 同名、namespace/name 前缀、ownerReference、Service/Endpoints | ✅ 正确 | 实测通过 |

---

## A. 租户注册的准入 webhook 作用于整个集群 ⛔

`apigroups.go` 把 `mutatingwebhookconfigurations` 与 `validatingwebhookconfigurations`
暴露给租户,而 `pkg/convert` 对它们**没有任何转换器**(落到 `defaultConvertor`,只改自身
name 和 ownerReference)。于是:

- webhook 的 `rules` **不带任何租户范围**,`namespaceSelector` 为空 ⇒ **匹配全集群**
- `clientConfig.service.{name,namespace}` **不加租户前缀** ⇒ 指向平台的命名空间

**实测**:租户 111111 注册一个 `failurePolicy: Fail`、`clientConfig` 指向不存在服务、
仅匹配 `configmaps` 的 ValidatingWebhookConfiguration 之后 ——

```
租户 222222 建 ConfigMap:
  Internal error occurred: failed calling webhook "hook.example.com": ...
                           service "nonexistent" not found
平台自身建 ConfigMap:
  Internal error occurred: failed calling webhook "hook.example.com": ...
```

**一个租户的一个对象,同时打死了另一个租户和平台自身。** 删掉该 webhook 后立即恢复。

影响不止 DoS:把 `failurePolicy` 改成 `Ignore` 并指向租户自己可达的服务,该 webhook
就能**读到全集群每一个被创建的对象** —— 跨全部租户的数据外泄。

⚠️ 注意 **per-namespace RBAC(#87)挡不住这条**:webhook configuration 是集群级资源,
而 RBAC 的 `resourceNames` 是精确匹配,表达不了"名字以 `<id>-` 开头"。

**修法(需决策)**:① 直接不向租户暴露这两个资源(最简单,与 kubezoo 的 KAaaS 定位相符);
② 或在转换层强制注入 `namespaceSelector`(限定到该租户的 namespace 标签)并改写
`clientConfig.service`。②依赖租户不能改写该字段,需要同时挡住 update。

## B. PersistentVolume 完全未改写 ⛔

`init.go` 把 PV 映射到 `nopeConvertor` —— **什么都不做**。而读路径按 `<id>-` 前缀过滤。
两边不对称,结果是三件事同时发生:

| 操作 | 实测结果 |
|---|---|
| 111111 创建 PV `shared` | 上游得到**裸名** `shared`,无租户前缀 |
| **创建者自己** `get`/`list` | **NotFound / 空** —— 连自己都看不见 |
| 222222 创建同名 PV | **`AlreadyExists`** —— 跨租户**存在性泄露** |
| 任何人删除 | NotFound ⇒ **对象永久滞留上游**,名字被永久占用 |

也就是说 PV 对租户既不可用又不可回收,同时还是一条跨租户信道。

## C. PVC 的 `spec.volumeName` 未改写 ⚠️

PVC 映射到 `defaultConvertor`,只改 namespace 与 ownerReference。**实测**:租户写
`volumeName: some-volume`,上游原样是 `some-volume`。

配合 B,PV↔PVC 绑定这条链路**整条没有转换**。这正是 TODO 点名要查的路径。

## D. 两个转换器写好了但从未接线 ⚠️

`NewPVTransformer`、`NewPVCTransformer` 在 `pkg/convert` 里实现完整、**各自的单元测试也都通过**,
但 `init.go` 从未注册它们:

```
NewPVTransformer     被引用 0 次
NewPVCTransformer    被引用 0 次
```

这是 B 和 C 的直接成因,也是本次审计方法学上最值得记住的一条:
**单元测试测的是转换器本身,没有任何测试检查它是否被接上。**

> 顺带:`init.go` 里还留着 `policy/PodSecurityPolicy` 的条目,该 kind 在 1.25 已被删除。
> 无害但陈旧。

## E. Node 对所有租户可见 ⚠️

**实测**确认:租户 `kubectl get nodes` 能列出平台节点。
成因是 `pkg/util/util.go` 里为通过 Conformance 加的 TODO 分支。已记在 TODO 1.2,取舍不是难题。

## 通过的项(正向对照)

均为**实测**:

- **namespace / name 前缀**:租户看到 `default/test`,上游是 `111111-default/test`
- **CRD 同名不冲突**:两租户各建 `widgets.stable.example.com`,上游分别是
  `widgets.111111-stable.example.com` 与 `widgets.222222-stable.example.com`
- **Service / Endpoints**:租户视角 `default/web`,上游 `111111-default/web`,转换器工作正常
- **跨租户 ownerReference**:指向另一租户对象的 namespaced ownerReference 会悬空并被 GC 回收
  —— k8s 本身禁止跨 namespace 的 owner,所以这条不构成通道
- **上游 RBAC 兜底**(#87 引入):跨租户 namespaced 访问被上游拒绝,已带负向对照

## 尚未覆盖

诚实列出,不算做完:

- **配额三条**(架构文档 §9)仍是**读码结论,未做运行时复现**:生效范围、
  `objectSelector` 标签绕过、`UpdateQuotaStatus` 空实现导致的并发超发
- watch(含 `resourceVersion=0` 全量)、label/field selector、`kubectl auth can-i`、
  SA token 换权、discovery/OpenAPI 泄露 —— 未测
- `Pod.spec.runtimeClassName` / `Ingress.spec.ingressClassName` 未改写导致的**悬空引用**:
  读码可见,未实测。架构文档已就 runtimeClass 定过"只能由平台强制注入"
- `Pod.spec.priorityClassName`:PriorityClass 未对租户暴露,但该字段不改写,
  租户可引用平台的 PriorityClass 抬高调度优先级 —— 读码结论,未实测
