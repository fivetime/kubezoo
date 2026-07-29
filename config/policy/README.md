# 租户准入策略(策略层)

这些**不是 kubezoo 的代码**,是**策略层**要执行的约束。放在本仓库是因为
**测试环境必须带上它们** —— 少了它们,lab 里跑的就不是完整形态,
而下面每一条不生效时都是一条实测可用的越权。

归属判据见 `docs/kaaas-platform-architecture-cn.md` §8.0:
准入只有写路径、碰不到响应 ⇒ 需要租户**看到**翻译后视图的事归 kubezoo;
只在写路径且**换个平台会变**的归这里。

## ⭐ 铁律:匹配一律反向写

**`exclude` 平台自己的 namespace,匹配其余全部。**

禁止用正向 selector 选租户 namespace 的标签 —— 除非那个标签**租户改不动**。
`kubezoo.io/tenant` 恰好是这种(kubezoo 无条件重写,四种摘法实测都失败),
但**排除法更稳**:新增一个租户 namespace 时不依赖任何标签就已经被覆盖。

配额组件违反过这条:用 `objectSelector` 按 `app` 标签排除自己的 Pod,
而标签是租户提交对象的一部分 —— 超额 Pod 抄上那个标签就放行了。

## 策略清单

| 文件 | 作用 | 不生效时 |
|---|---|---|
| `tenant-platform-classes.yaml` | 删掉租户设的 `runtimeClassName` / `priorityClassName` / `priority` / `ingressClassName` 及废弃的 `ingress.class` 注解 | 租户可跑出沙箱、可拿 `system-cluster-critical` 抢占全集群 |
| `tenant-deny-daemonset.yaml` | 拒绝租户建 DaemonSet | 租户可往平台每个节点投放 Pod |

## ⚠️⚠️ 两个会让策略"Ready 但什么都不做"的坑(都实测踩过)

### 1. `patchStrategicMerge` 里的 `null` 会被 apiserver 剪掉

Kyverno 文档里删字段的写法是置 `null`。但存进 CRD 字段时 **`null` 被剪掉了**,
实测存下来的是:

```json
{"patchStrategicMerge":{"spec":{}}}
```

策略 `READY=True`、`rulecount.mutate=2`,**而它什么都不做**。
⇒ 所以这里用 **`patchesJson6902`**(`op: remove` 能真删)。

### 2. 用了 JSON6902 就没有 autogen,pod controller 必须自己列全

Kyverno 的 `autogen` **只从 `patchStrategicMerge` 派生**。用 JSON6902 时
`.status.autogen` 是空的 —— 实测现象是:**Deployment 的模板里 `runtimeClassName: kata`
原样留着**,只有它生出来的 Pod 过准入时才被清掉。
"跑起来的东西"是对的,但**存下来的对象在撒谎**,而且这依赖"Pod 也会过一遍准入"这个间接性质。

⇒ 所以本目录里 pod controller 的规则是**显式写全的**:
7 个控制器走 `/spec/template/spec`,`CronJob` 多一层单列,`PodTemplate` 再单列。

### 3. `op: remove` 打在不存在的路径上会怎样

⚠️ 这条最危险:`failurePolicy: Fail` + patch 失败 = **所有 Pod 都建不了**。
实测**不会** —— 一个字段都没设的普通 Pod 照常创建。但改这些策略后**必须重测这一条**。

## ⚠️ 写这些策略时踩过的坑

1. **`runtimeClassName` / `priorityClassName` 在 `PodSpec` 里,PodSpec 嵌在 9 个 kind 里**。
   Kyverno 的 `autogen` 会从一条 Pod 规则自动派生出 pod controller 的规则,
   但它覆盖的是 **7 个控制器 + Pod = 8 个,不含 `PodTemplate`**
   (`pkg/autogen/v1/autogen.go`)。只写 Pod 而不靠 autogen,会漏掉 **Deployment 这条最常见的路径**
2. **`spec.priority` 要跟 `priorityClassName` 一起清** —— 只清名字会留下直接写进去的数值
3. **废弃的 `kubernetes.io/ingress.class` 注解要跟 `ingressClassName` 一起删** ——
   多数 ingress 控制器仍认它,只清字段等于没清

### 4. 空更新不触发准入(验策略时最容易骗到自己)

`kubectl label --overwrite` 把标签设成**和当前一样的值**,kubectl 发出的 patch 是空的,
apiserver 直接短路,**mutate/validate webhook 根本不会被调用**。看起来就像策略没生效。
验证时务必改成一个**不同的值**。

### 5. 多条策略并存时,别把拒绝归错策略

一个对象可能同时违反好几条规则,谁先拒的不一定是你在测的那条。实测踩过:测调度策略时
Deployment 被拒,以为规则生效了,实际是 `kubectl create deploy` 的默认模板不满足
`restricted`,拒它的是 pod security 那条。
**判据是拒绝消息里的 `策略名: 规则名`**,并且要把被测对象改成**只剩下被测的那一处违规**。

## ⚠️ 部署注意

### 装上策略之后,必须做一次存量修正 + 存量清理

策略只在准入时生效,对**已经存在**的对象一律不追溯:

```bash
# 存量 namespace 的 PSA 标签(策略装上之前建的不会被自动钉回)
kubectl label ns -l kubezoo.io/tenant pod-security.kubernetes.io/enforce=restricted --overwrite
# 存量违规 Pod —— 实测:补完 namespace 标签后,已在跑的 privileged Pod 仍然 Running,
# 只发了一条 warning。不主动删就等于没封。
```


- `failurePolicy` 用 `Fail`,并配多副本 + PDB。`Ignore` 的失效是**静默的**,直接击穿隔离前提
- ⛔ **`forceFailurePolicyIgnore` 环境变量能一次性把所有策略变成 `Ignore`**
  (`pkg/toggle/toggle.go`)。必须锁死并纳入巡检,否则配置成 `Fail` 只是纸面上的
- 能用 CEL 表达的,也可以考虑 `MutatingAdmissionPolicy`(1.36 已 GA 且默认开,
  跑在 apiserver 进程内、无单点),代价是**没有 autogen**,那 8~9 个 kind 要逐个手写路径
