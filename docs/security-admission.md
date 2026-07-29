# 安全与准入:kubezoo 管什么,不管什么

> 本文只讲**控制面**:租户经 kubezoo 使用 Kubernetes API 时,哪些约束由谁强制。
> 节点加固、运行时沙箱、网络隔离属于数据面,不在这里,见平台侧文档。

## 0. 前提:租户直接用 K8s API

这是 kubezoo 存在的理由,也是它的威胁模型的全部来源:

- 每个租户拿到自己的 kubeconfig,用**原生 kubectl**,看到的是一套**完整的 Kubernetes API 视图**
- 租户在自己的视角里是 admin:建 namespace、建 CRD、建 RBAC、建准入 webhook
- kubezoo 把这套视图**翻译**成上游共享集群里带 `<租户ID>-` 前缀的对象

⚠️ **不要把这份文档和"租户不碰 API"的模型混起来**。那种模型里租户只提工单、拿 IP、
像 VM 一样 ssh 进去,平台自己生成 YAML,租户 namespace 里**不放任何 RoleBinding** ——
那套的安全重心在容器逃逸与节点加固。**kubezoo 恰好相反**:租户是**已认证的 API 用户**,
攻击面是 API 本身,而 per-namespace RoleBinding 是**有意下发**的纵深防御(见 §2)。

## 1. 威胁模型

攻击者 = **一个拿到合法 kubeconfig 的租户**,不是应用被 RCE 之后的进程。

| 攻击者尝试 | 没有对应措施时会发生什么 |
|---|---|
| 直接指名道姓访问别的租户的 namespace | 上游 RBAC 拒绝(#87 起) |
| 建**集群级**对象撞名 / 探测存在性 | 名字前缀是唯一防线,**RBAC 表达不了前缀** |
| 建准入 webhook | 未收口时**全集群生效**,能打死其他租户和平台 |
| 建 ClusterRole 给自己提权 | `escalate`/`bind` 动词就是那条豁免 |
| 引用平台的集群级对象(RuntimeClass / PriorityClass / IngressClass) | 拿到平台的运行时(可跑出沙箱)或全集群最高优先级 —— 由策略层拦(`config/policy/`) |
| 读发现面 / OpenAPI | 枚举出其它租户的 id、CRD 组名与 schema |
| 摘掉自己 namespace 上的平台标签 | 策略匹配与退租清理同时失效 |
| 从自己 Pod 的 `spec.nodeName` 枚举平台节点名 | ⚠️ **当前可行,未修**(见 §6)—— 配合 `kubernetes.io/hostname` 可做定向共驻 |
| 退租时留下永不完成的 finalizer | namespace 永久 Terminating,租户 ID 不可复用 |

上面每一条都在 `isolation-audit-cn.md` 里有实测记录(含负向对照),多数已修。

## 2. 责任划分判据

完整推导见 `kaaas-platform-architecture-cn.md` §8.0,结论:

**准入(webhook / VAP / MAP)只见 AdmissionRequest,碰不到响应。**
所以需要租户**看到**翻译后视图的事 —— list/get/watch 返回、discovery、OpenAPI、
错误文案里的前缀擦除 —— 策略层**结构上做不了**。而前缀化天生双向(写时能加、读时无法去),
**寻址必须在 kubezoo**。

| 这条规则… | 归属 |
|---|---|
| 需要租户看到翻译后的视图 | **kubezoo**(唯一能) |
| 只在写路径,且**换个平台会变** | **策略层** |
| 只在写路径,但保护的是**寻址边界** | **kubezoo**(集群级资源无 RBAC 兜底) |

## 3. kubezoo 当前强制了什么(均已实测)

| 措施 | 位置 |
|---|---|
| namespace / name / CRD 组名前缀翻译,双向 | `pkg/convert` |
| 上游 RBAC **按 namespace 下发**,集群级逐资源指定动词 | `pkg/controller/rbac.go` |
| 不授 `escalate`/`bind`;`nodes/proxy` 等显式拒绝清单 | 同上,附守卫测试 |
| 租户 webhook 收口:`clientConfig` 命名空间加前缀、`namespaceSelector` 强制本租户、rule `scope` 强制 Namespaced、拒绝 `clientConfig.url` | `pkg/convert/webhookconfiguration.go` |
| Node 对租户不可见(list/get/watch 三条路径) | `pkg/util` + `pkg/proxy` + `pkg/convert` |
| OpenAPI v2/v3 按租户过滤与去前缀 | `pkg/filters` |
| access review 的 namespace / 组 / 主体转换 | `pkg/convert/accessreview.go` |
| 退租强制清理 finalizer | `pkg/controller/teardown.go` |
| 停机两种模式(read-only / frozen),前门 + 上游 RBAC 两层 | `pkg/filters/suspension.go` + 控制器 |

⚠️ **集群级资源没有第二道防线**:RBAC 的 `resourceNames` 是精确匹配,表达不了
"名字以 `<租户ID>-` 开头"。这一层写错就是直接越权 —— 审计里 PersistentVolume、
准入 webhook、Node 三条都出在这条路上。

## 4. 策略层必须补的(尚未部署)

| 约束 | 现状 |
|---|---|
| `runtimeClassName` / `ingressClassName` / `priorityClassName`(含 `spec.priority`)由平台决定 | ✅ **策略已写并实测** `config/policy/` |
| 拒绝 DaemonSet | ✅ **策略已写并实测**(租户被拒;平台自己的 DaemonSet 不受影响) |
| PSA `restricted` 等价规则 | ✅ **策略已写并实测** `config/policy/tenant-pod-security.yaml`。⚠️ **必须用 Kyverno 的 `validate.podSecurity`,不能用原生 PSA** —— 见下 |
| 拒绝 `spec.nodeName`、约束 `tolerations` | ✅ **策略已写并实测** `config/policy/tenant-scheduling.yaml`(审计 §O) |
| 落点:平台**注入**池子的 `nodeSelector`/`toleration`/`topologySpreadConstraints`,并**拒掉租户自写**的 `nodeSelector`/`affinity` | 未做(形态见架构 §8.2.2;⛔ 别用 required 反亲和,别用白名单) |

⛔ **原生 PSA(namespace 标签 `pod-security.kubernetes.io/enforce`)在这里是废的**:
kubezoo 只钉死 `kubezoo.io/tenant` 一个标签,其余标签原样转发上游,所以租户
**自己就能把自己的 namespace 标成 `privileged`** —— 建 ns 时带上、或事后 `kubectl label`,
两条路实测都拿到了 **Running 的 privileged + hostNetwork Pod**(审计 §N)。
又是"判定条件建立在租户可控输入上"那个形状。策略层按 `kubezoo.io/tenant` 匹配才立得住;
原生 PSA 降级为**兜底**(由策略把标签钉回 `restricted`)。

⭐ **策略在 `config/policy/`,lab 默认安装 Kyverno 并应用它们** —— 没有它们的 lab
测的不是完整形态,而上面每一条不生效时都是一条实测可用的越权。
⚠️ 那个目录的 README 记了两个会让策略 **"Ready 但什么都不做"** 的坑,改策略前务必看。

⭐ **优先用 `MutatingAdmissionPolicy`(1.36 已 GA 且默认开)而不是 webhook**:
它跑在 apiserver 进程内,没有 webhook、没有 `failurePolicy`、**没有单点**。
代价是**没有 autogen**,PodSpec 那 8~9 个 kind 要逐个手写路径(`CronJob` 多一层)。

⚠️ 若确实用 Kyverno:`forceFailurePolicyIgnore` 环境变量能**一次性把所有策略变成 Ignore**。
必须锁死并纳入巡检,否则配置成 `Fail` 只是纸面上的。

## 5. ⭐ 策略层依赖 kubezoo 提供的不变量

两层不是并列,是**依赖**。

策略层看到的是**改写后的上游对象**(namespace 是 `111111-default`),要按租户匹配
只能靠 `kubezoo.io/tenant` 标签 —— **而这个标签租户摘不掉,是 kubezoo 保证的**
(`kubectl label ns X 键-`、merge patch 置 null、json patch remove、改成别的租户 id,
四种写法实测上游一字未变)。

由此得到一条写策略时的**铁律**:

> **排除条件只能建立在租户改不动的东西上。**

配额组件违反过这条:它用 `objectSelector` 按 `app` 标签排除自己的 Pod,而标签是租户
提交对象的一部分 —— 超额 Pod 打上那个标签就**创建成功并落地上游**。改成按 namespace
排除后失效(namespace 不归租户控制)。

## 6. 明确的边界(不做,不是没做)

| | 为什么 |
|---|---|
| 数据面的一切(节点加固、沙箱、网络) | 不是控制面的事 |
| 取证快照 | 快照要能说清"哪个时间点/哪个 revision 的视图",停机机制给不了这个保证 |
| 节点级硬冻(`cgroup freezer`) | 节点级带外操作,kubezoo 碰不到 cgroup |
| 节点名从 `spec.nodeName` 漏给租户 | ⚠️ **不是"不做",是待定**:泄漏面未摸全,且 `-o wide` 显示 NODE 是 kubectl 常规行为,藏掉会碎掉排障体验。等 B1 节点池方案定了一起处理(架构 §8.2.3) |
| `Frozen` 中和租户自建的 RoleBinding | 控制面冻结管不到容器里已在跑的代码;租户可预埋 dead-man switch,换 VM/kata 也堵不住。⚠️ **所以 `Frozen` 不能单独当取证冻结用** |

## 7. 核对清单

上线前逐条验,**每条都要带负向对照**(确认测试真的走到了被测分支)——
这个项目在这上面栽过不止一次。

- [ ] 两个租户建同名的集群级对象(PV / CRD / RuntimeClass),互不可见、互不冲突
- [ ] 租户 A 指名访问租户 B 的 namespace → Forbidden
      ⚠️ **别看错误文案**:`TrimTenantIDFromError` 会把前缀从消息里擦掉,看着像没加前缀。
      **判据是上游落地的对象名**
- [ ] 租户建 `failurePolicy: Fail` 的 webhook 后,**另一个租户和平台**的操作不受影响
- [ ] 租户建 `*` on `*` 的 ClusterRole 并绑给自己 → 上游判定仍无权
- [ ] `kubectl get nodes` 为空,`kubectl get node <名字>` NotFound,watch 无事件
- [ ] 租户 A 的 `/openapi/v2` 与 `/openapi/v3` 里没有租户 B 的任何痕迹
- [ ] 四种写法都摘不掉 `kubezoo.io/tenant` 标签
- [ ] 租户留 finalizer 后退租 → namespace 仍能终结,租户 ID 可复用
- [ ] 租户新建一个 namespace 后,能在里面正常创建对象(RoleBinding 自动下发)
- [ ] 停机 `ReadOnly` / `Frozen` 期间 **Pod 保持 Running、restarts 不变**;解除后租户恢复可写
- [ ] 配额:超额 Pod 加上任意租户可控的标签后**仍被拒**
- [ ] 租户把自己 namespace 标成 `pod-security.kubernetes.io/enforce: privileged`(建时带 + 事后 patch 两条路),
      privileged / hostNetwork Pod **仍被拒**
      ⚠️ **改标签验策略时要改成不同的值**:设成同值是空更新,apiserver 短路,准入根本不跑
- [ ] 租户设 `spec.nodeName` 或容忍控制面污点 → 被拒;**干净 Pod 仍能建**
      ⚠️ 多条策略同时在时,**判据是拒绝消息里的策略名**,不是"被拒了"
- [ ] 装完策略做过一次存量修正(namespace 标签)**和存量清理**(已在跑的违规 Pod
      不会被自动干掉,实测只发 warning)

## 相关文档

- `isolation-audit-cn.md` —— 逐条findings、实测记录与负向对照
- `kaaas-platform-architecture-cn.md` §8 —— 职责划分判据、策略层选型、必备策略清单
- `../TODO-kaaas.md` —— 分阶段待办与已下的决定
