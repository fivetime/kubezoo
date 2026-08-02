# PodSpec 逐字段判定:谁在守,守得住吗

做法与 `PersistentVolumeClaimSpec` 那一轮相同 —— 把 spec 的每个字段逐个判定,
而不是凭印象。PVC 那次 9 个字段里查出一个活的逃逸(`volumeAttributesClassName`)。
PodSpec 有 **47 个字段**。

判定针对 **kubezoo 1.36 目标版本**。租户 Pod 建在上游集群的 `<租户ID>-<namespace>` 里。

---

## 0. 先说三个决定一切的结构性事实

### ⛔ ① kubezoo 对 Pod **一个字段都不碰**

`pkg/convert/` 里**没有 `pod.go`**。Pod 只走 `DefaultConvertor`:namespace 加前缀、
ownerReference 转换。除此之外 spec 原样透传。

### ⛔ ② kubezoo **从不跑对象校验,也从不跑准入**

`tenantProxy` 的每一个 `rest.ValidateObjectFunc` / `rest.ValidateObjectUpdateFunc`
参数都是 `_` —— 全部丢弃。租户对象的校验和准入**全部发生在上游 apiserver**。
这不是疏漏,是这个架构的形状(kubezoo 不是 `genericregistry.Store`)。
但它意味着:**凡是靠准入守的东西,守它的组件都在 kubezoo 之外。**

### ⛔ ③ 因此 `--allow-privileged` 在 kubezoo 上是**空操作**

`cmd/kubezoo/app/server.go` 里 `capabilities.Initialize(AllowPrivileged: ...)` 看着像
安全开关。但在 1.36 里读 `capabilities.Get().AllowPrivileged` 的只有
`pkg/apis/core/validation/validation.go:8798` 和 `:9013` —— 而 kubezoo 从不调用那个包。
同结构里的 `HostNetworkSources` / `HostPIDSources` / `HostIPCSources` 更是**在 1.36 里
彻底没人读**(PSP 时代遗留)。

⇒ **把 `--allow-privileged` 设成 false 不会拦住任何租户 Pod。** 别把它当防线。

---

## 1. 判定表

**守方**一栏的含义:

- `native` = 上游 apiserver **进程内**执行,不走 webhook,没有单点
- `kyverno` = Kyverno **webhook**,单点,挂了就没了
- `—` = 没人守

### C 类:直接夺取宿主机

| 字段 | 守方 | 机制 |
|---|---|---|
| `hostNetwork` / `hostPID` / `hostIPC` | **native + kyverno** | PSA Baseline;`tenant-pod-security` 的 `pin-psa-label` 把租户 namespace 的 PSA 标签钉成 restricted,原生 PSA 在 apiserver 进程内执行 |
| `securityContext.privileged`(容器级) | **native + kyverno** | 同上 |
| `volumes[].hostPath` | **native + kyverno** | 同上 |
| `securityContext.sysctls` | **native + kyverno** | 同上 |
| `nodeName` | ⚠️ **kyverno only** | `tenant-scheduling`(ClusterPolicy) |
| `pods/binding` 子资源 | **native** | `tenant-deny-binding`(**VAP**) |

⭐ 这一类**看着**是最好的一类:PSA 那几条**有意做了两层**,
`tenant-pod-security.yaml` 里的注释写明了原因:"原生 PSA 在 apiserver 进程内,
不走 webhook、没有单点。万一 Kyverno 挂掉…已建 namespace 上的标签仍然拦得住。"

⛔⛔ **lab 有一个结构性局限,已经踩到三次,写在这里**:Kyverno 在场时,
**几乎每条断言都分不清是哪一层干的** —— 两层都会把结果弄成对的。三次分别是:
PSA 标签(两层都会打)、`nodeName` 拒绝(已有断言要求必须是 Kyverno 拒的,kubezoo 先拒
导致健康集群上失败)、以及最阴的一次:我加了一条断言想验"非规范 placement 的 Pod 仍可写",
**它是空转的** —— 因为 Kyverno 的 `place-pod` 对**任何人**在租户 namespace 里建的 Pod
都生效,那个"遗留 Pod"在写入瞬间就被规范化了。**是负向对照(把缺陷放回去重跑)发现的,
不是断言本身。**

⇒ **规则:凡是声称验证 kubezoo 这一层的 lab 断言,必须跑一次负向对照。** 能区分层次的
只有两类:① 策略结构上够不到的路径(如模板的 UPDATE,策略只匹配 CREATE);
② 绕过 kubezoo 直接对上游操作(验的是策略那一层)。其余一律靠单测钉。

⚠️ 但"两层"这个说法**只对已经建好的 namespace 成立** —— 见 §2③,
装第二层的动作本身是第一层做的。`nodeName` 则是这一类里唯一的纯单层项。

### B 类:指向平台的集群级对象 / 平台级开关

| 字段 | 守方 | 机制 |
|---|---|---|
| `nodeSelector` | ⚠️ **kyverno only** | `tenant-placement` 替换成 `kubezoo.io/pool: <租户ID>` |
| `affinity` | ⚠️ **kyverno only** | `tenant-placement` 移除 |
| `tolerations` | ⚠️ **kyverno only** | `tenant-placement` 替换 |
| `topologySpreadConstraints` | ⚠️ **kyverno only** | `tenant-placement` 移除 |
| `schedulerName` | ⚠️ **kyverno only** | `tenant-placement` 替换 |
| `priorityClassName` / `priority` | ⚠️ **kyverno only** | `tenant-platform-classes` 移除 |
| `runtimeClassName` | ⚠️ **kyverno only** | `tenant-platform-classes` 移除 |
| `resourceClaims[]`(DRA) | 天然 | 只能引用**同 namespace** 的 claim;且 kubezoo 不服务 `resource.k8s.io`,租户建不了 claim。见 #94 |

### A 类:namespace 内引用 —— 跟着 namespace 前缀走,天然安全

`serviceAccountName`、`imagePullSecrets`、`volumes[].secret/configMap/persistentVolumeClaim/projected/csi(nodePublishSecretRef)`、
`env[].valueFrom.{secretKeyRef,configMapKeyRef}`、`envFrom`。

**不用管。** 前缀化发生在 namespace 层,这些引用解析在租户自己的 namespace 里。

### D 类:与隔离无关

`restartPolicy`、`terminationGracePeriodSeconds`、`activeDeadlineSeconds`、`dnsPolicy`、
`dnsConfig`、`hostname`、`subdomain`、`setHostnameAsFQDN`、`hostAliases`、`readinessGates`、
`enableServiceLinks`、`shareProcessNamespace`、`preemptionPolicy`、`os`、`overhead`、
`automountServiceAccountToken`、`deprecatedServiceAccount`、`schedulingGates`、
`initContainers` / `containers` / `ephemeralContainers` 的常规字段、`resources`(受配额约束)。

### 新字段(以前没人分类过)

| 字段 | 门控 @1.36 | 判定 |
|---|---|---|
| `hostnameOverride` | **1.35 Beta 默认开** ⇒ **活的** | 覆盖 Pod 主机名。作用域在 Pod 自己的 netns 内,不构成跨租户影响。**结论:无需处理**,但记下来它是新的、且默认开 |
| `hostUsers` | `UserNamespacesSupport` **1.36 GA 默认开** ⇒ **活的** | 见 §2④ —— 这是个**机会**,不是漏洞 |
| `schedulingGroup` | `GenericWorkload` 1.35 Alpha 默认**关**(1.37 才 Beta,仍默认关) | 潜伏,今天字段会被丢弃 |
| `evictionResponders` | `EvictionRequestAPI` **1.37 才有** | 1.36 不存在 |

---

## 2. 结论

### ✅ ① 落点隔离是**单层**,而且那一层**已经静默失效过一次** —— 已修

B 类整组 —— `nodeSelector` / `affinity` / `tolerations` / `topologySpreadConstraints` /
`schedulerName` / `priorityClassName` / `runtimeClassName`,加上 C 类里的 `nodeName` ——
**只有 Kyverno webhook 一层**。

这要紧,是因为落点原则里写着:**注入的 nodeSelector 才是承重件**。
租户的落点隔离就靠 `tenant-placement` 把 `kubezoo.io/pool: <租户ID>` 注进去。

而 `docs/operations-cn.md` 记着一次**真实事故**:一条我们自己的 VAP 拦住了 Kyverno
注册它自身的 webhook,三条策略永远不就绪,**`pods` 的 webhook 根本没注册**,
`hostNetwork` / `hostPID` / `nodeName` 全部放行 —— 而屏幕上只有几个 `READY=<none>`。

⇒ Kyverno 一挂,**租户 Pod 可以落到任何节点上,包括别的租户的节点池**。
C 类靠 PSA 那两层扛住了宿主机逃逸,但**落点隔离没有第二层**。

**✅ 已做**(选了 §3 的 (c),剥离 + 注入):

- 规则定义在 contract 一处(`NodePoolFor` / `NodePoolLabelKey` / `TenantSchedulerName` /
  `UnreachableTolerationSeconds`),`TestPlacementMatchesThePolicy` **双向**比对策略 YAML
- `pkg/convert/placement.go` 对 **9 种带 Pod 模板的 kind** 全部注入
- **前置**:平台节点也打污点(见 `docs/operations-cn.md` §2),
  这样万一还有漏网路径,故障模式是 Pending 而不是落错

⭐ **改模板才是关键,不是改 Pod。** kubezoo 看不见 Deployment 产出的 Pod ——
那些是 kube-controller-manager 直接对上游建的。但 kubezoo 看得见**租户写的模板**,
所以模板存对了,整个故障期间它就一直在产出落点正确的 Pod。这是对那批 Pod 唯一可行的保护。

⛔ **修正:Pod 只在 CREATE 放置,模板才是每次都放置。** 我第一版让 `place()` 在
**每次**写入都重写 Pod 的 placement,那是个缺陷:`spec.nodeSelector` / `schedulerName`
在 Pod update 上**不可变**,而 `validateOnlyAddedTolerations`(k8s `validation.go:4415`)
要求**已有的每一条 toleration 都必须还在**。于是对任何"存储的 placement 与规范值不同"
的 Pod —— 升级前建的、或 webhook 不在时建的 —— 租户**从此改不动自己正在运行的 Pod**。
Kyverno 没这个问题,因为它的规则匹配 `operations: [CREATE]`。

⇒ 现在:模板走转换器(无需 verb),Pod 走 `tenantProxy.Create` 的 `convert.PlacePod`。
放在**转换之后、构造 unstructured 之前**,这样 SSA 的 `conversionDelta` 能看见这些注入
字段并带上;放在那之后就会被 apply 丢掉。

⚠️ **kubezoo 在模板的 UPDATE 上也注入,策略只匹配 CREATE。** 策略之所以能只管 CREATE,
是因为 Pod 本身永远是 CREATE、`place-pod` 会把模板说的覆盖掉 —— 而那个兜底恰恰是
webhook 不在时缺失的那个。lab 里"改完模板还能被改回去"是**唯一一条能区分出是哪一层
干的**断言。

### ✅ ② 两条钉节点的路,韧性不同 —— 已修

`spec.nodeName`(Kyverno)和 `pods/binding`(原生 VAP)都被拦,但**只有后者是进程内的**。
lab 对两条都有断言,断言通过掩盖了它们韧性不同这件事。

**✅ 已做**,分成两条路,因为这个字段有个陷阱:

- **模板里的 `nodeName`:清除**(`pkg/convert/placement.go`)。模板里出现它一定是租户
  自己写的 —— Kubernetes 任何组件都不会往模板里写。清除而不是拒绝,是为了让一个已经
  带着它的 Deployment 继续 reconcile,而不是从此每次写入都失败。
- **Pod 上的 `nodeName`:在 CREATE 上拒**(`tenantProxy.refuseTenantChosenNode`)。

⛔ **不能在 UPDATE 上拦,这不是偏好而是唯一可行的规则**:**调度器会把
`spec.nodeName` 写到它绑定的每一个 Pod 上**,此后对该 Pod 的任何更新(改标签、加注解、
写 status)都带着它。在 update 上拒 = 集群里每个运行中的 Pod 从此写不动。
CREATE 是唯一能确定这个字段只可能来自租户的时刻。

⭐ 为什么这个字段值得单独对待:它**绕过调度器**,kubelet 因为 Pod 点了名就直接接收,
于是**所有活在调度器里的规则、首先是污点,压根不被查**。仍然生效的只有 nodeSelector
(kubelet 会检)—— 这正是"每租户专属的池**标签**"承重、而**光有污点不够**的原因。

### ✅ ③ C 类的"第二层"**是由第一层安装的** —— 已修

`pin-psa-label` 把 `pod-security.kubernetes.io/enforce: restricted` 打在租户 namespace 上,
之后由**原生 PSA(进程内)**执行 —— 这层确实独立于 Kyverno。

**但打这个标签的动作本身是一条 Kyverno mutate。**

⇒ 它守的是"Kyverno 先把标签打上、之后才挂掉";守不住"**namespace 创建时 Kyverno 就不在**"。
那种情况下 namespace 没有 PSA 标签,该 namespace 里的 `hostNetwork` / `privileged` /
`hostPath` **全部放行**,直到有人补打标签。

有人会说 `failurePolicy: Fail` 兜住了 —— **只兜住一半**。Fail 管的是"webhook 调用失败",
而那次真实事故是 **webhook 根本没注册**。**没注册的 webhook 不会失败,它压根不存在**,
`failurePolicy` 对它无效。这正是当时 `pods` 被全量放行的机制,
同样的机制会让新建 namespace 拿不到 PSA 标签。
(README 里还记着 `forceFailurePolicyIgnore` 环境变量能一次性把所有策略变成 `Ignore`。)

⭐ **而这条修起来几乎免费,因为该打标签的地方 kubezoo 已经在打标签了**:

- `pkg/convert/namespace.go` 的 `Forward` **已经**给每个租户 namespace 盖
  `kubezoo.io/tenant`,并且拒绝租户篡改它 —— 纯 Go,不依赖任何外部组件。
  在同一处补盖 PSA 标签即可。
- 控制器建租户 namespace 时(`kubezoo-controller` `controller.go:873`)同理,
  那里也只打了 tenant 标签。

两处都补上之后,C 类的第二层才**真正**独立于 Kyverno:标签由 kubezoo 自己写,
执行由 apiserver 进程内的 PSA 做,全程不经过任何 webhook。

**✅ 已做**:

- 值定义在 contract(`PodSecurityLevel` / `PodSecurityVersion`),并有一个**双向**守卫测试
  直接读 Kyverno 的 YAML 比对 —— 只看自己那个值的话,旁边写着 `enforce: privileged`
  也会通过。变异检查:策略改弱成 baseline → 红;版本钉死到旧版 → 红。
- `pkg/convert/namespace.go`:在已经盖 `kubezoo.io/tenant` 的同一处补盖 PSA 标签。
- 控制器 `syncNamespaces`:创建时盖,并纳入已有的漂移修复回路(**只在真的漂了才写**,
  否则每个租户 × 每个 namespace × 每轮 resync 都是一次写)。

⭐ **是覆盖,不是拒绝** —— 和上面那个 tenant 标签相反,是有意的。拒绝读着更严,
但更糟:一个真的被写弱了的 namespace(恰恰是在这次加固所针对的那种故障窗口里写弱的)
会变成**它的租户再也写不进去**、只能等管理员救的 namespace。覆盖则在下一次写入时自动修好。
tenant 标签之所以拒绝,是因为改它等于改**归属**;这个只是个级别,放回去不丢任何东西。

⚠️ 注意这仍然**不解决 ①** —— 落点隔离靠的是注入 `nodeSelector`,PSA 管不着。

### ⭐ ④ `hostUsers: false` 是白捡的纵深,今天没人在用

`UserNamespacesSupport` 在 **1.36 是 GA 且默认开**。`hostUsers: false` 给 Pod 开独立的
user namespace —— 这是**容器逃逸最实在的缓解手段**:容器里的 root 不是宿主机的 root。

kubezoo 和所有策略里,**没有任何地方设置它**。默认值(不设 = `true`)是**隔离更弱**的那个。

平台强制注入 `hostUsers: false` 是一条独立的、纯增益的加固。
⚠️ 但它有兼容代价:`spec.os.name: windows` 时不能设,某些需要宿主 user ns 的负载会坏
(加载内核模块之类 —— 而那类负载本来就该被 PSA 挡住)。

---

## 3. 第二步的设计岔路(需要定夺)

给 B 类加 kubezoo 侧第二层,有三种做法,**行为后果差别很大**:

| | 做法 | 兼容性 | Kyverno 挂掉时 |
|---|---|---|---|
| **(a)** | kubezoo **拒绝**设置了这些字段的 Pod | ⛔ **破坏性**:今天租户可以设(只是被静默替换掉),改成拒绝会当场打断他们 | 完全保护 |
| **(b)** | kubezoo **剥离**这些字段(与 Kyverno 同为静默) | ✅ 无破坏 | **部分保护**:租户写的没了,但**注入也没发生** ⇒ Pod 无约束,可落任意节点 |
| **(c)** | kubezoo **剥离 + 注入**(自己也写 `kubezoo.io/pool`) | ✅ 无破坏 | 完全保护 |

⭐ **(c) 是唯一真正解决 ① 的**,但它把节点池的规则**写在两个地方**(Kyverno 策略里一份、
Go 代码里一份),两份不一致就是下一个 bug。而 contract 的 README 里恰好写着这条原则:
策略与 Go 代码"是同一套规则的两种表达,只有来自同一个 release 才会一致"。

⇒ 若选 (c),节点池的推导规则应当**只在 contract 里定义一次**,Kyverno 策略和 Go 代码
都从那里取,而不是各写各的。
