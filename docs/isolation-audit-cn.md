# kubezoo 隔离正确性审计(#82)

对着**真实上游集群**(kind **v1.36.1** + kubezoo 移植版 + 本地 etcd)做的双租户黑盒审计,
不是读码结论。每条结论都注明了是**实测**还是**读码**。

审计横跨多个提交:起点 `e01e169`(per-namespace RBAC 落地),各节的"已修"指向各自的提交。
凡标注**未修 / 待定**的(I、部分 H)以本文为准。

## 结论摘要

| # | 问题 | 严重度 | 状态 |
|---|---|---|---|
| A | 租户可注册**全集群生效**的准入 webhook,打死其他租户与平台 | ⛔ 最高 | **已修并实测** |
| B | PersistentVolume 完全未改写:撞名 + 存在性泄露 + 对象永久滞留 | ⛔ 高 | **已修并实测** |
| C | PVC 的 `spec.volumeName` 未改写 | ⚠️ 中 | **已修并实测** |
| D | `PVTranformer` / `PVCTransformer` 写好了但**从未接线** | ⚠️ 中(B/C 的成因) | **已接线,并加接线守卫** |
| E | Node 对所有租户可见(**三处豁免,TODO 只点了一处**) | ⚠️ 中 | **已修并实测** |
| **F** | ⛔⛔ **租户可自建 `*` on `*` 的 ClusterRole 并绑给自己,完全废掉 #87** | ⛔ 最高 | **已修并实测** |
| **G** | ⛔⛔ **`nodes/proxy` 被授权 ⇒ 租户可达任意节点 kubelet API(完整逃逸)** | ⛔ 最高 | **已修并实测** |
| H | 配额:每 namespace 各一份额度 / `objectSelector` 标签可绕过 / 单点 | ⛔ 高 | **绕过已修,其余坐实** |
| **I** | ⛔ **`runtimeClassName` / `ingressClassName` / `priorityClassName` 两头错**:自己的引用不到,平台的引用得到(含 `system-cluster-critical`) | ⛔ 最高 | **策略层已实现并实测**(`config/policy/`,lab 默认装 Kyverno) |
| J | `kubectl auth can-i` 对租户全错(SAR 属性不转换) | ⚠️ 中 | **已修并实测**(集群级残留=vanilla 行为) |
| K | `serviceaccounts/token`、`pods/eviction`、`pods/binding` 解不出请求体 | ⚠️ 中 | **已修并实测** |
| L | `/openapi/v2` 原样透传 ⇒ 跨租户信息泄露 + 文档自相矛盾 | ⚠️ 中 | **已修并实测** |
| M | `/openapi/v3` 不含租户 CRD ⇒ `kubectl explain` 默认路径失效 | ⚠️ 低(不泄露) | **已修并实测** |
| — | CRD 同名、namespace/name 前缀、ownerReference、Service/Endpoints、**watch 过滤**、**field selector**、**发现面** | ✅ 正确 | 实测通过 |

---

## A. 租户注册的准入 webhook 作用于整个集群 ⛔ —— 已修复

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

### 已采用的修法:转换层强制改写(保留能力,收敛爆炸半径)

⚠️ 顺带查出**第二条同类路径**:CRD 的 `spec.conversion.webhook.clientConfig` 也没改写。
它在 CRD spec 里而不是 webhookconfiguration,**"不暴露 webhookconfigurations"堵不住它** ——
而 CRD 是 kubezoo 的核心能力,不可能不暴露。

⚠️ 还有一个事实决定了这个取舍的代价:**该能力此前对正当用途本来就不可用**。
`clientConfig.service.namespace` 不加前缀 ⇒ 租户指向自己的服务会落到平台的同名命名空间。
**能工作的只有滥用路径。** 所以"改写"删掉的是坏掉的行为,加回来的是第一次真正可用的能力。

四处强制,缺一即有出口(`pkg/convert/webhookconfiguration.go` + CRD 转换器):

1. `clientConfig.service.namespace` 加租户前缀 —— **同时让租户 webhook 第一次真正可用**
2. `namespaceSelector` 强制替换为 `kubezoo.io/tenant: <id>`
3. 每条 rule 的 `scope` 强制为 `Namespaced` —— ⚠️ **最容易漏的一条**:
   `namespaceSelector` **对集群级资源不生效**,不锁 scope 则 rule 写 clusterroles 仍然全集群触发
4. `clientConfig.url` **直接拒绝** —— URL 指向哪里由租户决定,没有任何办法把它收敛

**实测(同一个攻击原样重放)**:

| 检查 | 修前 | 修后 |
|---|---|---|
| 上游对象 service | `default/nonexistent` | `111111-default/nonexistent` |
| 上游对象 namespaceSelector | `{}`(全集群) | `{"kubezoo.io/tenant":"111111"}` |
| 上游对象 rules[0].scope | `*` | `Namespaced` |
| 另一租户建 ConfigMap | **被打死** | 正常 |
| 平台 default / kube-system | **被打死** | 正常 |
| 平台建 ClusterRole | 会被匹配 | 不再匹配 |
| **租户自己的 namespace** | 被拦 | **仍被拦**(证明 webhook 真生效,不是被架空) |

**逃逸尝试(均失败)**:`clientConfig.url` 直接被拒;`kubectl patch` 把
`namespaceSelector` 改回 `{}`、`scope` 改回 `*` —— 上游读回来仍是强制值(转换器在 update 也跑)。

**CRD conversion webhook 同样收口**:上游 `111111-default/conv`,租户读回 `default/conv`(往返对称);
`url` 形式同样被拒。

⚠️ **仍然成立的自伤面**:租户的 `failurePolicy: Fail` webhook 可以拦住**自己 namespace 内**的操作,
包括平台在该 namespace 里的协调(如 #87 的 RoleBinding)与清理。这是租户自伤,不是越权,
但会影响该租户 namespace 的可回收性。

## B. PersistentVolume 完全未改写 ⛔ —— 已修复

`init.go` 把 PV 映射到 `nopeConvertor` —— **什么都不做**。而读路径按 `<id>-` 前缀过滤。
两边不对称,结果是三件事同时发生:

| 操作 | 实测结果 |
|---|---|
| 111111 创建 PV `shared` | 上游得到**裸名** `shared`,无租户前缀 |
| **创建者自己** `get`/`list` | **NotFound / 空** —— 连自己都看不见 |
| 222222 创建同名 PV | **`AlreadyExists`** —— 跨租户**存在性泄露** |
| 任何人删除 | NotFound ⇒ **对象永久滞留上游**,名字被永久占用 |

也就是说 PV 对租户既不可用又不可回收,同时还是一条跨租户信道。

### 修法与实测

PV 由 `nopeConvertor` 改为 `defaultConvertor`(提供 name 前缀)+ `PVTransformer`
(改写 `spec.claimRef.namespace`)。重放同一场景:

| 检查 | 修前 | 修后 |
|---|---|---|
| 两租户各建同名 PV `shared2` | 第二个 `AlreadyExists` | `111111-shared2` / `222222-shared2` 各自成立 |
| 创建者 get / list | NotFound / 空 | 各自看到自己的 `shared2` |
| 跨租户 get / list / delete | —— | 全部 NotFound,且对象**未被误删** |
| 所有者 delete | 谁都删不掉 | 正常删除,上游零残留 |

⚠️ **迁移提示**:修复前产生的**裸名 PV 仍然滞留在上游**,且照旧对所有租户不可见。
它们在修复前后都是孤儿,需要运维手工清理 —— 这不是本次改动引入的。

## C. PVC 的 `spec.volumeName` 未改写 ⚠️ —— 已修复

PVC 映射到 `defaultConvertor`,只改 namespace 与 ownerReference。**实测**:租户写
`volumeName: some-volume`,上游原样是 `some-volume`。

配合 B,PV↔PVC 绑定这条链路**整条没有转换**。这正是 TODO 点名要查的路径。

### 修法与实测

PVC 由 `defaultConvertor` 改为叠加 `PVCTransformer`。实测:租户写 `volumeName: shared2`,
上游是 `111111-shared2`,租户读回仍是 `shared2` —— 往返对称。

## D. 两个转换器写好了但从未接线 ⚠️

`NewPVTransformer`、`NewPVCTransformer` 在 `pkg/convert` 里实现完整、**各自的单元测试也都通过**,
但 `init.go` 从未注册它们:

```
NewPVTransformer     被引用 0 次
NewPVCTransformer    被引用 0 次
```

这是 B 和 C 的直接成因,也是本次审计方法学上最值得记住的一条:
**单元测试测的是转换器本身,没有任何测试检查它是否被接上。**

**已接线,并新增两个"接线守卫"测试**(webhook 与 PV/PVC 各一),都验证过:
把注册项摘掉或改回 `nopeConvertor` 就会报红。

> 顺带:`init.go` 里当时还留着 `policy/PodSecurityPolicy`(1.25 已删除的 kind)与
> `scheduling.k8s.io/PriorityClass`(kubezoo 不服务)两个 `nopeConvertor` 条目。
> **随 E 一并删掉了** —— 它们永远匹配不上,但若哪天把 PriorityClass 服务出去,
> 那条 nope 会原样复现 B 的 bug。现在已经没有"什么都不做"的转换器条目。

## E. Node 对所有租户可见 ⚠️ —— 已修复

**实测**确认:租户 `kubectl get nodes` 能列出平台节点 —— 名字、标签、地址、容量,
以及 `status.nodeInfo` 里的内核版本 / 容器运行时 / kubelet 版本。
对租户来说这是**它与所有其他租户共用的那批机器的清单**,也是一份"哪里有 CVE 就查哪里"的索引。

### ⭐ TODO 只点了一处,实际有三处

按 TODO 删掉 `pkg/util/util.go` 那个 Conformance 分支之后**再测,`get nodes` 是空的了,
但 `get node <名字>` 照样返回完整对象** —— 因为 `pkg/proxy/proxy.go` 的 Get 路径里
**还有一个独立的 Node 豁免**,让集群级资源的名字前缀转换跳过 Node:

```go
if !tp.namespaceScoped && tp.kind.Kind != "Node" {   // ← 第二处
```

第三处在 `pkg/convert/init.go`:Node 映射到 `nopeConvertor`(完全不转换)。
**这与 B(PersistentVolume)是同一个形状**:`nopeConvertor` + 读路径按前缀过滤,
两边不对称。三处各带一条 TODO 注释,只 grep 注释文案只能找到一处。

### 修法与复测

三处一起去掉,Node 回归成一个普通的集群级资源(名字前缀说了算,平台节点都没有前缀):

| 路径 | 修前 | 修后 |
|---|---|---|
| `kubectl get nodes` | 列出平台节点 | `No resources found` |
| `kubectl get node <名字>` | **返回完整对象** | `NotFound` |
| `GET /api/v1/nodes/<名字>` | 完整对象 | 404 |
| `watch /api/v1/nodes` | 有事件 | 静默 |
| 平台自己看 | 正常 | 正常(未受影响) |

顺带把 `init.go` 里另外两个 `nopeConvertor` 条目也删了:`scheduling.k8s.io/PriorityClass`
(kubezoo 根本不服务)和 `policy/PodSecurityPolicy`(k8s 1.25 已删除的 kind)。
两个都永远匹配不上;但**如果哪天按 I 的 A 方案把 PriorityClass 服务出去,
那条 nope 会原样复现 PV 的 bug**。现在没有"什么都不做"的转换器条目了,
落到默认(加前缀)才是安全的兜底。

## F. 租户可以把 #87 的兜底整个拆掉 ⛔⛔ —— 已修复

`clusterScopedRules()` 对 rbac 资源授的是 `*` 动词。**`*` 包含 `escalate` 和 `bind`,
而这两个动词的唯一用途就是"允许提权"** —— RBAC 本来拒绝你创建超出自身权限的角色,
它们正是那条豁免。

**实测(修前)**:租户三步拆掉 #87 ——

1. 建 ClusterRole `escalate`,规则 `*` on `*` → 上游 `111111-escalate`,**创建成功**
2. 建 ClusterRoleBinding 把 `admin`(改写后 = 自己的上游身份 `111111-admin`)绑上去,**成功**
3. 上游判定:读 222222 的 secret → **yes**;读 kube-system → **yes**;全集群 list pods → **yes**

**根因定位靠对照组**:同样授予 clusterroles 资源、但**显式列出动词且不含 escalate/bind** 的用户,
第 1 步即 Forbidden。`auth can-i escalate clusterroles --as=111111-admin` → **yes**,坐实。

**修法**:rbac 资源的动词改为显式列出(get/list/watch/create/update/patch/delete/deletecollection),
**不含 escalate/bind**。修后重放:第 1 步 Forbidden;绑到平台现成的 `cluster-admin` 虽然创建成功,
但 **roleRef 被改写层改成 `111111-cluster-admin`(租户自己的窄角色)⇒ 绑了等于没绑** —— 两层防御都在。
正当用途保留:租户仍可创建自身权限范围内的 ClusterRole 并绑给自己的 SA。

## G. `nodes/proxy` = 完整租户逃逸 ⛔⛔ —— 已修复

**实测(修前)**:租户**经 kubezoo** 打 `/api/v1/nodes/<node>/proxy/pods`,
列出该节点上**全部** Pod,含 `kube-system` 的 etcd。由此可读任意 Pod 日志、exec 进任意容器。
绕过 kubezoo 直打上游同样成功。**这条路径是 proxy,改写层根本看不到 path**,所以纯靠上游 RBAC。

⭐ **根因是方法学**:`clusterScopedRules()` 当初是**机械地从 apigroups.go 的暴露面推导**的 ——
`nodes/proxy` 被服务,所以就被授权。**"授予一切被服务的"继承了暴露面里的每一个坏决定。**

**修法**:授权改为逐资源指定动词,并新增 `notGrantedToTenants` 显式拒绝清单:
`nodes/proxy`(本条逃逸)、`nodes/status`(写节点状态是 kubelet 的事)、
`namespaces/status` 与 `namespaces/finalize`(namespace 控制器的事;finalize 还能强推终结)。
守卫测试相应改形:**服务面 = 授权 ∪ 显式拒绝**,既保住同步性,又让拒绝读起来是决定而非遗漏。

修后重放:两条路径均 Forbidden。

⚠️ **kubectl 的坑**:`kubectl auth can-i get nodes/proxy` 的**斜杠写法会把 proxy 当成资源名**,
答 `yes` 是错的;`--subresource=proxy` 才对(答 `no`),实际请求也确实 Forbidden。
**判据要用实际请求,不能只信 can-i。**

## H. 配额三条 —— 真部署跑完,两条坐实一条未复现

此前全是读码结论。本轮把 `clusterresourcequota` 组件真正部署进 lab 跑通后逐条验。

⭐ **部署本身就先撞到移植期遗留的一个错误假设**:`NewQuotaConfigurationForAdmission(nil, nil)`
的两个 nil。我在 #83 记过"只有 DRA + DRAExtendedResource 同时开启才会解引用,没构造该场景测过" ——
**该假设是错的**:`DynamicResourceAllocation` 自 1.35 起 GA 且 `LockToDefault`,
`DRAExtendedResource` 在 **1.36 转 Beta 默认开**,那条分支必然走到,组件**一启动就 CrashLoop**。
修法:不塞一个用不上的全集群 Pod informer,而是**明确告诉配额配置 kubezoo 不服务 `resource.k8s.io`**
(apigroups.go 本来就没暴露),分支自然跳过。

### ① 生效范围:每个 namespace 各拿一份完整额度 ⛔ 坐实

租户声明 `cpu=4`,控制器在**每个**租户 namespace 里生成一份 `hard={"cpu":"4","memory":"4G"}`
—— 不是共享一份。实测:

```
111111-default / kube-node-lease / kube-public / kube-system   各 cpu=4  ⇒ 4 个 ns × 4 = 16 core
租户再自建 2 个 namespace 后                                    ⇒ 6 个 ns × 4 = 24 core(声明值的 6 倍)
```

**随 namespace 数无限增长**。租户总量约束只由 webhook 在准入时兜(见 ②),
per-namespace 的 ResourceQuota 对象本身完全不构成总量约束。

### ② `objectSelector` 标签绕过 ⛔ 坐实并已修

webhook 原本用 `objectSelector: app NotIn [kubezoo-cluster-resource-quota]` 排除自身 Pod,
**而标签是租户提交对象的一部分**。实测(经 kubezoo 的正常租户路径):

| 请求 | 修前 | 修后 |
|---|---|---|
| 超额 Pod,无标签 | 被拒 | 被拒 |
| **同一个 Pod + `app: kubezoo-cluster-resource-quota`** | **创建成功并落地上游** | **被拒** |

**修法:排除条件改为按 namespace**(`namespaceSelector` 排除 `default`/`kube-system`)——
**namespace 不归租户控制**:`pkg/convert` 在入站时改写 namespace 并拒绝租户冒用他人前缀,
所以租户对象永远落不到平台自己的 namespace 里。
同时验证平台组件删除后仍能自愈重建(排除仍然生效,webhook 没有自锁)。

> ⭐ 这正是 TODO 3.1 那条铁律的实例:**排除条件只能建立在租户无法控制的东西上。**

### ③ 并发超发:代码确认,本轮未复现

`quotaAccessor.UpdateQuotaStatus` 确实是空实现(`webhook.go:190` 直接 `return nil`),
关闭了 admission 期的乐观并发记账。但**并发 6 个 2-core Pod 打 4-core 配额,实际只落地 2 个,未超发**。

⇒ 记为**代码确认、行为未复现**。要坐实需要更高并发或更小时窗(本地单副本 webhook 串行度太高)。
**不写成"已坐实"。**

### ④ 单点:坐实

`replicas: 1` + `failurePolicy: Fail` + **无 PDB**。配额 webhook 挂掉时全租户 Pod 创建全部失败。

---

⚠️ **本节的方法学教训(踩了三次)**:配额类测试对环境状态极其敏感,我连续三次拿到无效对照 ——
① 测试脚本参数错位导致标签根本没加上;② 前一步删过 Pod 导致用量归零、放行是正确行为;
③ `nodeName: nonexistent-node` 让 Pod 被 k8s GC 回收,用量随时间漂移。
**每次都是"看起来得出了结论"。判据必须是:先确认起点状态,再做单一变量对照。**

## 通过的项(正向对照)

均为**实测**:

- **namespace / name 前缀**:租户看到 `default/test`,上游是 `111111-default/test`
- **CRD 同名不冲突**:两租户各建 `widgets.stable.example.com`,上游分别是
  `widgets.111111-stable.example.com` 与 `widgets.222222-stable.example.com`
- **Service / Endpoints**:租户视角 `default/web`,上游 `111111-default/web`,转换器工作正常
- **跨租户 ownerReference**:指向另一租户对象的 namespaced ownerReference 会悬空并被 GC 回收
  —— k8s 本身禁止跨 namespace 的 owner,所以这条不构成通道
- **上游 RBAC 兜底**(#87 引入):跨租户 namespaced 访问被上游拒绝,已带负向对照
- **watch 按租户过滤且做了反向转换**:租户 1 从 `resourceVersion=0` watch
  **集群级**的 runtimeclasses,窗口内平台和租户 2 各建一个,两个都没出现在流里;
  自己的对象以**去前缀**的名字出现(`myrc` 而非 `111111-myrc`)。
  ⚠️ 负向对照就是"窗口内别人确实写了" —— 没有这一步,一条空流说明不了任何事
- **namespace 名字花招无效**:租户 1 建名为 `222222-x` 的 namespace,
  上游落成 `111111-222222-x`,租户 2 看不到。
  ⚠️ 差点误判:租户请求 `-n 222222-default` 时报错文案写的是 namespace `"222222-default"`,
  看着像没加前缀 —— 那是 `TrimTenantIDFromError` 把 `111111-` 从**错误消息**里擦掉了。
  **错误文案不能当证据**,要看上游落地的对象名
- **field selector 不构成通道**:`metadata.namespace=222222-default` 在自己 ns 内查,返回空
- **租户摘不掉自己 namespace 上的 `kubezoo.io/tenant` 标签**(这条是 A 的 webhook 收口
  与退租强制清理**共同的地基**,所以专门验了)。四种写法全试:
  `kubectl label ns probe kubezoo.io/tenant-` / merge patch 置 null / json patch remove /
  改成别的租户 id —— 上游标签**一字未变**,改成别人的 id 直接被拒。
  转换器在 create 和 **patch**(走 `guaranteedUpdate` → `update`)上都跑,所以补不上洞。
  ⚠️ 小瑕疵:前两种写法 kubectl 回显 "unlabeled" / "patched" **像是成功了**,实际没变;
  再 `get` 一次能看到真相
- **CRD 发现面隔离**:租户 1 建 `widgets.acme.io`(上游 `widgets.111111-acme.io`),
  租户 2 的 `api-resources --api-group=acme.io`、`get crd`、`/apis`、`/openapi/v3` 全为空
  —— 但 `/openapi/v2` 泄露,见 L 节

## I. 三个"按名引用集群级对象"的字段 ⛔ 坐实

这三条此前都是读码结论。本轮在双租户 lab 里逐条跑通,**结论比读码更重**:
不只是"悬空引用"这一半,另一半是**租户可以引用平台的同类对象**。

集群级资源按**名字**加前缀,而引用它们的字段**原样透传**,于是同一个名字
在两个方向上都错:自己的引用不到,别人的引用得到。

| 字段 | 引用自己的 | 引用平台的 |
|---|---|---|
| `Pod.spec.runtimeClassName` | ⛔ **建不出来** | ⛔ **成功** |
| `Ingress.spec.ingressClassName` | ⚠️ **静默失效** | ⛔ **成功** |
| `Pod.spec.priorityClassName` | (不暴露 PriorityClass) | ⛔ **成功** |

### ① RuntimeClass:自己的用不了,平台的用得上

租户建 `RuntimeClass/myrc` → 上游是 `111111-myrc`。随后:

```
# 引用自己刚建的:
Error from server (Forbidden): pod rejected: RuntimeClass "myrc" not found
# 引用平台的(租户 get 它是 NotFound,看都看不见):
pod/p-platform-rc created     上游 runtimeClassName=kata
```

**这一条对 B1 架构是承重的**:kata 是计算隔离边界,而 RuntimeClass 名字空间
对租户是"看不见但可用"的全局空间 —— 租户写 `runtimeClassName: runc`
(或任何平台其它 handler)就跑在沙箱外。RuntimeClass 还带
`scheduling.nodeSelector` 与 `overhead`,引用平台的类同时把平台的节点选择器一起带上。

### ② IngressClass:同样两头错,而且是静默的

租户建 `IngressClass/myic` → 上游 `111111-myic`;租户的 Ingress 里
`ingressClassName: myic` **原样落到上游**,指向一个不存在的类。
Ingress **没有准入期存在性校验**,所以对象创建成功、读回来一字不差、
永远没有控制器认领 —— 比 RuntimeClass 那个响亮的 Forbidden 更难查。

反向:`ingressClassName: platform-nginx` 直接接上平台的 ingress 控制器。
到这一步 host/path 归属就只由控制器裁决了,kubezoo 不参与。

### ③ priorityClassName:可以拿到全集群最高优先级

PriorityClass **根本没对租户暴露**(`api-resources` 里没有,LIST 报 404),
但字段不改写,于是:

```
priorityClassName: platform-high            → 上游 priority=1000000
priorityClassName: system-cluster-critical  → 上游 priority=2000000000
```

`system-cluster-critical` 不再有"仅限 kube-system"的准入限制
(已核对 1.36 源码 `plugin/pkg/admission/priority/admission.go:359` 的
`resolvePriorityClass`,只查存在性)。**任一租户可以把自己的 Pod 抬到
全集群最高优先级,抢占其它租户的负载。**

### 定案:**这三个字段是平台的决策面,租户设置无效** —— 但**由策略层执行**

考虑过给引用加前缀(并为每个租户投影一份平台共享类),**否决了** ——
前缀化会让租户**永远用不了平台共享类**(kata / nginx),而那恰恰是唯一正确的用法。

**这三个字段不该由租户决定**,平台用什么手段决定(默认类 / 准入注入)是平台的事。
按架构文档 §8.0 的判据,执行**属于策略层**:它只在写路径(丢了不需要反向还原),
而"负载该用哪个 runtimeClass"**换个平台就会变**,放进 Go 代码意味着改策略要发版。

### 现状:**由策略层执行,已实测**

曾经在 kubezoo 里实现过一版,**已按职责划分删除** —— 那是策略层的活。
策略现在在 `config/policy/tenant-platform-classes.yaml`,**lab 默认装 Kyverno 并应用它**,
所以测试环境跑的是完整形态。

实测(租户侧提交,看上游落地):Pod / **Deployment 模板** / **CronJob 模板**(嵌两层)/
Ingress(字段 + 废弃注解)**全部清空**,无关注解 `keep-me` 保留;
⭐ 负向对照:**平台自己 `kube-system` 里的 Pod 保留 `runtimeClassName: kata`**,没有被误伤。

### 迁到策略层时,三个当时踩过的坑不要再踩

1. **`runtimeClassName` / `priorityClassName` 在 `PodSpec` 里,而 PodSpec 嵌在 9 个 kind 里**
   (Pod / PodTemplate / RC / Deployment / ReplicaSet / StatefulSet / DaemonSet / Job / CronJob)。
   只处理 Pod 会漏掉 **Deployment 这条最常见的路径**,而且看起来像做完了。
   ⚠️ **Kyverno 的 autogen 只覆盖 8 个(不含 `PodTemplate`)**;MAP **没有 autogen**,要逐个写,
   `CronJob` 还多一层(`spec.jobTemplate.spec.template.spec`)
2. **`spec.priority` 要跟 `priorityClassName` 一起清** —— 只清名字会留下直接写进去的数值
3. **废弃的 `kubernetes.io/ingress.class` 注解要跟字段一起删** —— 多数控制器仍认它,
   只清字段等于没清

## J. `kubectl auth can-i` 对租户全错 ⛔ 坐实

SubjectAccessReview 的 `spec.resourceAttributes.namespace` **不经转换**直接送上游。

```
租户:  kubectl auth can-i create pods -n default   → no
租户:  kubectl run ... -n default                  → pod/cani-control created
上游:  can-i create pods -n 111111-default --as=111111-admin → yes
上游:  can-i create pods -n default        --as=111111-admin → no
```

四行放在一起才是判据:**答案是"no",动作却成功**,而上游对转换后/未转换的
namespace 分别给出 yes/no —— 定位到未转换的那一个。

⚠️ **#87 之前这条是看不见的**:那时租户在上游是 `*` on `*` 集群级,
问任何 namespace 都回 yes,恰好"看起来对"。收紧权限才让它显形。
这类缺陷会跟着每一次权限收紧冒出来,不是 #87 引入的 bug 而是 #87 揭出的。

### 修法与复测

新增 `pkg/convert/accessreview.go`,四个 kind 全接线(`SelfSubjectAccessReview` /
`LocalSubjectAccessReview` / `SubjectAccessReview` / `SelfSubjectRulesReview`),
转换 `resourceAttributes` 的 namespace 与**自定义资源组**(原生组不动),
并把主体搬进租户身份空间:SA 用户名的 namespace 加前缀,
**裸 `system:` 主体直接拒绝** —— 那是平台的身份,拿它提问等于读平台 RBAC。

复测:`can-i create pods -n default` 现在回 **yes**,与实际动作一致。

⚠️ **一个诚实的残留,且已证明是 vanilla 行为**:对**集群级**资源,
kubectl 仍会带上当前 namespace,而租户在自己 namespace 里是 `*` on `*`,
于是 `can-i get nodes --subresource=proxy` 回 **yes**,真实请求却 Forbidden。
**对照实验**:在上游直接给一个普通 user 绑上同样的 namespaced `*` on `*`,
`can-i get nodes --subresource=proxy -n <该 ns> --as=plain-user` 同样回 yes
⇒ **这是原生 k8s 行为**(kubectl 自己会打印 "resource is not namespace scoped" 警告),
不是 kubezoo 引入的。要比 vanilla 更准,得在 `resourceAttributes` 里按资源判断作用域后清空 namespace,
而 `resourceAttributes` 只有复数 resource 没有 kind —— 记为待办,不在本轮。

## K. 两个子资源解不出请求体 ⛔ 坐实

`apigroups.go` 里子资源沿用父资源的 Kind,而**请求体不是父资源**的那些就崩:

```
kubectl create token robot
  → TokenRequest in version "v1" cannot be handled as a ServiceAccount:
    converting (v1.TokenRequest) to (core.ServiceAccount): unknown conversion
POST pods/<name>/eviction
  → Eviction in version "v1" cannot be handled as a Pod: unknown conversion
```

`scale` 不受影响,因为它本来就有专门的 `groupVersionKindForScale`。
所以这不是"没想到",而是**只给 scale 想到了**。

影响:1.24 之后 `kubectl create token` 是取 SA token 的唯一方式;
`eviction` 是 PDB 生效的路径,也就是说租户侧任何优雅驱逐都走不通。
(Pod 内的 projected token 由上游 kubelet 签发,不走这条路,所以工作负载本身不受影响。)

⭐ 顺手挖出**同源的第二层**:`pods/binding` 也是同一个错(body 是 Binding),
一共三个。而且改完 body kind 之后 `create token` **仍然失败**,报 `name is required` ——
子资源的父对象名字,`pkg/dynamic` 是从 **body 的 `metadata.name`** 里取的。
eviction / binding 的 body 按惯例带名字所以看不出来,**TokenRequest 不带**。
名字本来就在**请求路径**里。已加 `CreateSubresource(ctx, name, ...)` 显式传名,
`Create` 保持原行为委托给它。

### 复测(全部实跑)

```
kubectl create token robot   → JWT,sub=system:serviceaccount:111111-default:robot
POST pods/<n>/eviction       → {"kind":"Eviction","apiVersion":"policy/v1"}
POST pods/<n>/binding        → Conflict: pod already assigned(解码错误消失,是正常业务冲突)
```

⚠️ 附带观察:token 的 `sub` 是**上游 namespace**(`111111-default`)。
token 由上游签发且签名覆盖 payload,改写会使其失效 ⇒ **不改**。
这只让租户知道自己的 id(不跨租户),记录在案。

## L. `/openapi/v2` 原样透传 ⛔ 坐实

`/openapi/v2` 是上游文档**未过滤、未转换**地发给每个租户:

```
租户 222222 拉 /openapi/v2,里面有:
  /apis/111111-acme.io/v1/namespaces/{namespace}/widgets
  io.111111-acme.v1.Widget
```

带对照:租户 222222 自己的 CRD 也是以 `222222-beta.io` 出现的 ——
**两边都是上游名字**,证明这条路径上一次转换都没发生,不是"漏了某个租户"。

两个后果:

- **信息泄露**:任一租户能枚举出所有其它租户的 id、CRD 组名、Kind 与 schema
- **文档自相矛盾**:定义键被改了名,**路径、`$ref`、
  `x-kubernetes-group-version-kind` 三处没改** ⇒ ref 指向不存在的定义

### 修法与复测

先**按归属删**(只有顶层 paths / definitions 的键能判断归属),
再**整篇文本剥掉本租户前缀**(删干净之后,剩下的每一次出现都是前缀,
无论它在键里、`$ref` 里还是扩展字段里)。两个编码走同一段逻辑。

⚠️ **中途踩到一次,值得记**:先写的版本只改键不改体,
protobuf 那路又"只剥自己不删别人" —— 因为 gnostic 的 `Document`
把 paths 表示成**具名条目数组而不是 JSON 对象**,基于 map 的删除**静默无事可做**,
文本剥离却照跑 ⇒ 结果正好反过来:自己的前缀没了,别人的原封不动。
**"函数跑了"不等于"函数做了事"**,判据只能是输出。

复测(两租户互为对照,JSON 与 protobuf 两种编码各测):

| | 租户 111111 | 租户 222222 |
|---|---|---|
| 自己的路径 | `/apis/acme.io/v1/widgets` ✅ 去前缀 | `/apis/beta.io/v1/gadgets` ✅ |
| 对方的路径/定义 | 0 条 ✅ | 0 条 ✅ |
| 残留 `111111-`/`222222-` | 0 / 0 ✅ | 0 / 0 ✅ |
| 悬空 `$ref` | 0 ✅ | 0 ✅ |
| 原生面 `/api/v1/pods` | 在 ✅ | 在 ✅ |

`kubectl explain widget --output=plaintext-openapiv2` **现在能用了**
(`KIND: Widget / VERSION: acme.io/v1`),证明 v2 文档确实自洽了。
但默认的 `kubectl explain` **仍然失败** —— 那是另一条,见 M。

## M. `/openapi/v3` 里根本没有租户的 CRD ⚠️ —— 已修复

kubezoo 的 `/openapi/v3` 是它**自己聚合的静态文档**(34 个 path,全是原生组),
**从不把上游的 CRD schema 并进来**。两个租户拉到的 v3 逐字节相同。

- 隔离上**没问题**:里面没有任何租户的东西,自然不泄露
- 功能上**有问题**:`kubectl explain` 默认走 v3 ⇒ 对租户**自己刚建、
  且 `kubectl get` 完全正常**的 CR 报
  `couldn't find resource for "acme.io/v1, Resource=widgets"`

### 修法:两半分别取,而且必须分别取

v3 是一个索引(`/openapi/v3`)加每个 group-version 一份文档。

- **原生那一半继续用 kubezoo 自己的** —— 上游的索引描述的是**上游 apiserver**,
  里面有 kubezoo 根本不服务的资源(如 `resource.k8s.io`),
  照抄等于把它们广告给租户
- **自定义那一半只能来自上游** —— schema 只存在于那里。按归属过滤 + 剥前缀,
  与 L 同样的两道,原因也一样

实现上索引这一半用 `responseRecorder` 把下游 handler 的输出接住再合并,
这样这个 filter 不需要知道 kubezoo 那份是怎么建出来的。
上游取不到时**降级为只回原生面**并打日志 —— 那一半仍然正确,
而且正是本次修复之前的行为,整个请求失败反而更糟。

### 复测(两租户互为对照)

| 检查 | 租户 111111 | 租户 222222 |
|---|---|---|
| 索引 path 数 | 34 → **35** | 34 → **35** |
| 新增的那条 | `apis/acme.io/v1` ✅ | `apis/beta.io/v1` ✅ |
| `serverRelativeURL` | 去了前缀、保留上游 hash ✅ | 同 ✅ |
| 索引里残留前缀 | 0 ✅ | 0 ✅ |
| 原生 `api/v1` / `apis/apps/v1` | 在,且是 kubezoo 自己那份 ✅ | 同 ✅ |
| 取自己的 GV 文档 | 72KB,残留前缀 0,`gvk.group=acme.io` ✅ | ✅ |
| 取**对方**的 GV 文档 | —— | **404** ✅ |
| 原生 GV 文档 | 仍由 kubezoo 自己服务 ✅ | ✅ |

**验收(就是 M 的定义)**:

```
kubectl explain widget            → GROUP: acme.io / KIND: Widget / VERSION: v1
kubectl explain widget.spec.size  → FIELD: size <string>
租户 222222 explain widget        → the server doesn't have a resource type "widget"
kubectl explain deployment.spec.replicas → 原生仍正常
```

服务给租户的 schema 与上游那份**逐字段相同**(含 `description`),只差前缀。

⚠️ **差点误判**:改完第一次测,`explain` 仍然报一样的错 ——
**kubectl 把 openapi 文档缓存在 `~/.kube/cache`**,拿的是旧的。
`rm -rf ~/.kube/cache` 之后才是真结果。**客户端缓存会让服务端的修复看起来没生效。**

## N. 原生 PSA 被租户一个 namespace 标签整个绕开 ⛔ 坐实

**起因**:待办里"PSA `restricted` 等价规则"一直挂着,以为只是没写。真去写的时候才发现
**选哪一种实现是有对错的** —— 原生 PSA 在这里根本拦不住租户。

原生 PSA 的判定输入是 namespace 标签 `pod-security.kubernetes.io/enforce`。
而 `pkg/convert/namespace.go` 的 `Forward` **只钉死 `kubezoo.io/tenant` 一个标签**,
其余标签原样转发上游 —— 包括这一个。

### 实测(上游全局 `AdmissionConfiguration` 已把默认值设成 `restricted`)

| 步骤 | 结果 |
|---|---|
| 正向对照:平台自己在普通 ns 建 privileged Pod | **Forbidden**,PSA 确实在强制 |
| 租户建 ns `plain`(不带标签),再建 privileged+hostNetwork Pod | **Forbidden** ✅ |
| 租户建 ns 时自带 `enforce: privileged` | 上游 ns **拿到该标签** |
| ↑ 然后在里面建 privileged+hostNetwork Pod | ⛔ **`pod/p created`,Running,落在控制面节点上** |
| 租户事后 `kubectl label` 把已有 ns 改成 `privileged` | ⛔ 同样成功,`pod/p2 created` |

两条路径都能拿到 **privileged + hostNetwork 且真的在跑**的容器。
这和配额组件当年栽的是**同一个形状**:判定条件建立在**租户能控制的输入**上
(那次是 `objectSelector` 按 `app` 标签排除,租户打上那个标签就绕过)。

### 修法:Kyverno `validate.podSecurity`,按租户改不动的标签匹配

`config/policy/tenant-pod-security.yaml`,匹配条件是
`namespaceSelector: kubezoo.io/tenant Exists` —— **不依赖任何租户可控输入**。

复测(ns 标签仍是 `privileged` 的前提下):

| | 结果 |
|---|---|
| 租户建 privileged Pod | **denied**,`validate.kyverno.svc-fail` |
| 控制器路径:Deployment 里塞 privileged | **denied**(`validate.podSecurity` 有 autogen,不吃 JSON6902 那个坑) |
| 负向对照:合规 Pod | **created** |

另加一条 `pin-psa-label` 规则把租户 namespace 的 PSA 标签钉回 `restricted`,
让**原生 PSA 反过来给 Kyverno 兜底** —— 它在 apiserver 进程内,不走 webhook、没有单点,
Kyverno 挂掉或被 `forceFailurePolicyIgnore` 一把变成 Ignore 时仍然拦得住。

### ⭐ 顺带测出来的三件事,部署时都会咬人

**① 空更新不触发准入。** `kubectl label --overwrite` 设成**和现在一样的值**,
patch 是空的,apiserver 短路,mutate webhook 根本没被调用 —— 我第一次就是这样看到
"钉不回去",差点又归因错。改成一个**不同的值**立刻钉回。
**判据:改标签验策略时,一定要改成不同的值。**

**② 策略装上之前就带坏标签的 namespace 不会被追溯修正。** `plain` 一直是 `privileged`。
上线时要补一次:

```bash
kubectl label ns -l kubezoo.io/tenant pod-security.kubernetes.io/enforce=restricted --overwrite
```

好在主规则不依赖这个标签,所以陈旧标签**没有把洞重新打开** —— 实测 `plain` 里再建
privileged Pod 仍被 Kyverno 拒。兜底层过期只是少一层,不是漏一个。

**③ 准入只管门口,不追溯。** 补完标签之后,之前漏进来的两个 Pod
**至今仍在 Running,仍是 privileged + hostNetwork**;重新打标签只发了条 warning。
封堵之后必须**主动清理已经进来的东西**,否则等于没封。

## O. 租户指定节点:`spec.nodeName` 与 `tolerations` ✅ 已由策略层拦住

`config/policy/tenant-scheduling.yaml`,两条 deny,匹配条件同样是
`namespaceSelector: kubezoo.io/tenant Exists`。

| | 结果 |
|---|---|
| 负向对照:干净合规 Pod | **created** |
| `spec.nodeName` 指名节点 | **denied** — `tenant-scheduling: deny-nodename` |
| 容忍 `node-role.kubernetes.io/control-plane` | **denied** — `tenant-scheduling: restrict-tolerations` |
| 控制器路径:Deployment 模板里塞同样两样 | **denied**(autogen 覆盖) |
| 负向对照:干净 Deployment | **created** |

### 三个坑

**① `tolerations` 不能一刀切。** `DefaultTolerationSeconds` 是**进程内**的 mutating 插件,
在 webhook 之前就给每个 Pod 加上两条容忍。实测干净 Pod 上确实带着:

```
node.kubernetes.io/not-ready     NoExecute
node.kubernetes.io/unreachable   NoExecute
```

写成"不许有 tolerations"会**拒掉每一个 Pod**。规则必须白名单这两个 key。

**② 规则只能匹配 `CREATE`。** 调度器绑定走 `pods/binding` 子资源,但绑完之后
`spec.nodeName` 就有值了 —— 规则若也匹配 `UPDATE`,任何后续改动(加个标签)都会被自己拒掉。
实测已调度的 Pod `kubectl label` 仍然成功,说明范围收对了。

**③ 多条策略同时生效时,别把拒绝归错。** 第一次测控制器路径,Deployment 被拒了,
但拒它的是 `tenant-pod-security-restricted`(`kubectl create deploy` 的默认模板本身不合规),
不是我要测的 `tenant-scheduling`。**判据是拒绝消息里的策略名和规则名**,不是"被拒了"。
把模板改成完全合规、只留下被测的那一处违规,才测到真东西。

## P. 节点名从 `spec.nodeName` 漏给租户 ✅ **定案:接受,不改**

⚠️ **这条是测 §O 时顺带看到的,不是设计过的测试** —— 观测本身可靠(以租户
kubeconfig、用租户视角的 namespace 名读取),但**泄漏面没有系统摸过**。

```
# KUBECONFIG=<租户 111111>
kubectl -n escape get pod c1 -o jsonpath='{.spec.nodeName}'
→ kz-audit3-control-plane          # 平台真实节点名
```

代码侧确认:`pkg/convert` / `pkg/proxy` / `pkg/util` 里 **一处都没碰过 `nodeName`**。

于是现状是自相矛盾的:**Node 对象藏了 list/get/watch 三条路径(§7.1),
但节点名字从每一个 Pod 上漏出来**。租户建几个 Pod 就能枚举平台节点名,
再配合 `nodeSelector: kubernetes.io/hostname` 做**定向共驻**。

### ⭐ 顺带纠正一个直觉:"看不到 Node ⇒ 设了也无效" 不成立

评估 `nodeSelector` / `tolerations` / `affinity` 的是**调度器** —— 它在上游、
用自己的凭据读真实 Node 对象,**根本不查租户能看到什么**。可见性是 kubezoo 在
**读路径**上做的,而调度发生在写入之后,两条路不相交。

代码确认 `pkg/` 里 **一处都没碰过** `nodeSelector` / `tolerations` / `affinity` /
`topologySpreadConstraints`,原样透传上游。而且 `nodeSelector` **不需要知道节点名**,
它匹配的是标签,标准标签全世界一样(`node-role.kubernetes.io/control-plane`、
`topology.kubernetes.io/zone`、`kubernetes.io/hostname`)。

⚠️ 未实测的一条:"平台给节点打了污点时,租户的 `tolerations` 确实能让它上去" ——
本轮 lab 是单节点 kind,没有可用的污点场景,**只测到策略把它拒了**。要坐实得有多节点 lab。

### ✅ 定案:接受这个泄漏,不改(用户 2026-07-29 决定)

> 「即使租户 `describe pod` 看到节点名也没用,他的设置会被替代掉。」

**理由**:知道节点名**只在能拿它做事的时候才值钱**。按 §8.2.2 的原则,
落点字段(`nodeSelector` / `tolerations` / `affinity` / `topologySpreadConstraints` /
`schedulerName`)全部由平台替换 ⇒ 名字是一条**兑现不了的信息**。
反过来,藏掉 `spec.nodeName` 会让 `kubectl -o wide` 和 `describe pod` 对租户失真,
排障体验碎掉 —— 代价确定,收益为零。

⚠️ **成立条件(必须盯住)**:替换要**覆盖到 `pods/binding`**。
binding 不是"设置",它是**直接写节点名的一次写入**,替换够不到 ——
那正是"知道名字"唯一能兑现的地方(§Q)。
好在 kubelet **会**核对注入的 `nodeSelector`,所以只要
**那个标签每租户专属**,跨租户 binding 会被 kubelet 拒,名字就彻底作废。
⇒ **这条定案的有效性绑在"注入的 nodeSelector 标签每租户专属"上。**

次要且被接受的残余:节点名会泄漏平台的命名规则、规模、可用区分布 ——
属于信息披露,不是访问权;租户本来就有专属节点池,意义有限。

### 归属(若将来要改)

按 §8.0 判据这是**读路径 ⇒ kubezoo 的**,策略层结构上够不着(准入看不到响应)。
真要改还得先摸全泄漏面:`-o wide`、`status` 里、events、PV 的 `nodeAffinity`。

## Q. `pods/binding` 绕开落点控制 ⛔ **已实测坐实**(但被注入的 nodeSelector 兜住)

> 原为读码推论,**2026-07-29 多节点 lab 实测坐实**,详见 §R。推论的每一步都对。

三件已确认的事实拼在一起:

**① 租户在自己 namespace 里是 `*` on `*`。**
`pkg/controller/rbac.go:270`,`kubezoo:tenant-namespace-admin` 就是
`NewRule("*").Groups("*").Resources("*")` —— 有意为之(见那里的注释:RoleBinding 只能在
自己 namespace 内授权,而且要覆盖租户 CRD 的自定义资源)。
但 `*` on `*` **包含 `create` on `pods/binding`**。

**② `pods/binding` 在 k8s 里是留给调度器的。**
`plugin/pkg/auth/authorizer/rbac/bootstrappolicy/policy.go` 里,
`create pods/binding` 只出现在 `kubeSchedulerRules`(`system:kube-scheduler`),
内置的 `admin` / `edit` 都没有。

**③ 我们的 `deny-nodename` 规则匹配的是 `kinds: [Pod]`。**
Binding 是**另一个 kind**、走**子资源**路径 —— 规则不会匹配到它。

⇒ 推论:租户先建一个不带 `nodeName` 的 Pod(过准入),再往 `pods/binding` POST 一个
Binding **直接指定节点**,就绕开了 `deny-nodename`。

### 为什么这不只是"绕过一条规则"

**kubelet 的准入只检 `NoExecute` 污点。** k8s 源码 `pkg/kubelet/lifecycle/predicate.go:448`
写得很直白:

```go
// Check taint/toleration except for static pods
// Kubelet is only interested in the NoExecute taint.
return t.Effect == v1.TaintEffectNoExecute
```

`NoSchedule` 污点**只有调度器**会检。直接 binding 跳过调度器 ⇒
**`NoSchedule` 污点被完全绕开**。若 B1 的 kata 节点池只用 `NoSchedule` 隔离,
这条路可能直接把节点池隔离打穿。

(`nodeSelector` / `nodeAffinity` 反而**仍然有效** —— kubelet 的 `generalFilter`
里有 `PodMatchNodeSelector`。)

### ⭐ 兜住这条路的是注入的 `nodeSelector`,不是污点

节点池方案(架构 §8.2.2)里平台会给租户 Pod **注入**该租户池的 `nodeSelector`。
它在建 Pod 时就焊进 spec(Pod spec 建后基本不可变,租户事后拆不掉),
而 kubelet **会**核对它 ⇒ 租户 A 把 Pod bind 到租户 B 的节点上,**会被 kubelet 拒**。

这跟直觉是反的:平时污点是"硬"机制、选择器是"软"的;在 binding 这条路上正好倒过来。
⇒ **注入的 `nodeSelector` 是承重件**,而且它是否承重取决于
**那个标签只有该租户的节点才有** —— 用一个所有池子共有的标签就等于没兜住。

⚠️ 所以多节点 lab 要测的**决定性一条**是:平台注入 nodeSelector 之后,
跨租户的 `pods/binding` 是不是真被 kubelet 拒。

### 还不知道的(这条能不能成立全看这些)

- **kubezoo 到底代不代理 `pods/binding`?** Binding 是"子资源 + 请求体是另一个 kind"
  的典型,正是 §K 那类问题。可能根本解不出请求体就报错了 —— 那样这条自动不成立
- 真绑上去之后 Pod 会不会被 kubelet 拒(资源、nodeSelector)
- 平台若用 `NoExecute` 而非 `NoSchedule` 打污点,kubelet 就会拒 —— **这可能就是修法**

### 怎么测

需要**多节点 lab**(单节点 kind 没有可用的污点场景):一个带 `NoSchedule` 污点的节点,
租户建 Pod 后 POST binding 指向它,看是否 Running。
⭐ 这和 §P 里"污点存在时租户 `tolerations` 确实能上去"是**同一个 lab**,一起测。

### 归属

若成立,修法有三条路,判据不同:

| 修法 | 归属 |
|---|---|
| Kyverno 规则加上 Binding / `pods/binding` 子资源 | 策略层(写路径) |
| 平台改用 `NoExecute` 打污点 | 平台基础设施 |
| 收窄 `kubezoo:tenant-namespace-admin`,把 `pods/binding` 排除掉 | **kubezoo**(它保护的是寻址边界,且集群级/子资源无 RBAC 兜底) |

⚠️ 第三条要小心:那个 `*` on `*` 是**有意的**(覆盖租户 CRD),改成排除列表就等于
维护一份黑名单 —— 又是"排除条件"那个形状,得先想清楚新增子资源时谁来补。

## R. 落点「注入替换」可行性实测 ✅ 可行(kubezoo + Kyverno)

**问题**:§8.2.2 那条一句话原则 —— 「租户看不到节点,没有任何调度权,他写的东西会被
平台替换掉」—— kubezoo 配 Kyverno 到底做不做得到?

**lab**:3 节点 kind(control-plane + 2 worker),每租户一个池子:

```
kz-audit3-worker    label kubezoo.io/pool=111111   taint kubezoo.io/pool=111111:NoSchedule
kz-audit3-worker2   label kubezoo.io/pool=222222   taint kubezoo.io/pool=222222:NoSchedule
```

策略 `config/policy/tenant-placement.yaml`,租户 ID 从 namespace 前缀派生
(`split(request.namespace, '-') | [0]` —— 前缀由 kubezoo 强制,租户伪造不了)。

### ① 替换本身:✅ 全中

租户 111111 提交一个**敌意** Pod(指向 222222 的池子 + 控制面容忍 + affinity + spread):

| 字段 | 租户写的 | 上游实际落地 |
|---|---|---|
| `nodeSelector` | `pool: 222222` | **`pool: 111111`** |
| `tolerations` | 222222 池 + **控制面** | **111111 池 + `not-ready` + `unreachable`(300s)** |
| `affinity` | required 到 222222 | **无** |
| `topologySpreadConstraints` | 有 | **无** |
| `schedulerName` | — | `default-scheduler` |
| 实际落点 | — | **`kz-audit3-worker`(自己的池子),Running** |

- **租户 ID 是真派生的**:222222 提交同一份,落到 `worker2`
- **控制器路径同样有效**:Deployment 的 Pod 也落在自己池子
- ⚠️ 用了 JSON6902 就**没有 autogen**,9 个 kind 全部显式写(README 坑 2)

### ② ⚠️ 预测过的冲突,原样复现

准入链是**所有 mutating 跑完再跑 validating**。先前那条
`tenant-scheduling: restrict-tolerations`(白名单 validate)把**平台自己注入**的
池子容忍拒掉了:

```
denied ... tenant-scheduling: restrict-tolerations:
Only the tolerations Kubernetes adds itself are allowed
```

⇒ 那条 deny 已删除(它本就是节点池方案落地前的过渡)。
**教训:注入型策略上线时,必须同时清掉针对同一字段的验证型策略。**

### ③ ⛔ `pods/binding` 确实绕得过去(§Q 坐实)

把 111111 的池子 `cordon` 掉让 Pod 卡在 Pending,租户直接 POST binding 到 **222222 的节点**:

```
kubectl create -f binding.json --raw /api/v1/namespaces/default/pods/bindme/binding
→ {"kind":"Binding",...}          # kubezoo **确实代理**这个子资源,没报错
上游 bindme: node=kz-audit3-worker2      # 真的绑上了另一个租户的节点
```

- **kubezoo 代理 `pods/binding`** —— §Q 的前置疑问解决:它不是"解不出请求体"
- **`NoSchedule` 污点没拦住** —— 与 kubelet 源码一致(只看 `NoExecute`)
- Kyverno 的 `deny-nodename` 匹配 `kinds: [Pod]`,**匹配不到 Binding**

### ④ ✅ 但注入的 `nodeSelector` 兜住了,而且**前提已实测**

```
phase  = Failed
reason = NodeAffinity
msg    = Pod was rejected: Predicate NodeAffinity failed:
         node(s) didn't match Pod's node affinity/selector
```

容器一个都没起。**兜住它的是注入的 nodeSelector,不是污点。**

⭐ **负向对照(这条最重要)**:把 `worker2` 也标上 `pool=111111`(模拟"所有池子共有的标签"),
同一次 binding —— Pod **Running 在另一个池子的节点上**。

⇒ **「注入的池子标签每租户专属」不是优化项,是承重前提。** 这条现在是量出来的。

### 结论与残余

**可行。** kubezoo + Kyverno 能实现那条原则,但成立依赖两件事,都要写进部署检查单:

1. **池子标签每租户专属**(见上,负向对照已证)
2. **注入型策略上线时清掉同字段的验证型策略**(见 ②)

### ⑤ ⛔ Kyverno 堵不住 binding,原生 VAP 可以

想在 **API 层**堵掉 binding,先试 Kyverno 的子资源匹配 `kinds: [Pod/binding]`:

| 观察 | |
|---|---|
| 策略状态 | `Ready=True`,`rulecount.validate=1` |
| webhook 注册 | ✅ `validate.kyverno.svc-fail  res=['pods/binding', ...]  ops=['CREATE']` |
| 实际效果 | ⛔ **binding 照样成功** |
| Kyverno 日志 | **连这次请求都没有** |

经 kubezoo、直连上游两条路都试过,去掉 `namespaceSelector` 也一样 ——
**这是本轮第四次"Ready 但什么都不做"**。

换成 **k8s 1.36 原生的 `ValidatingAdmissionPolicy`**(进程内、无 webhook、无单点,
正是 §8.5 一直偏好的形态):

```
租户 POST binding →
  ValidatingAdmissionPolicy 'tenant-deny-binding' denied request:
  binding a pod to a node is the scheduler's job; tenants hold no scheduling authority
  nodeName = ''            # 一点都没绑上
```

⚠️ **表达式必须写成"只放行调度器"**(`request.userInfo.username == 'system:kube-scheduler'`),
不能写成无条件拒绝 —— 调度器绑定走的是**同一条子资源**,写死拒绝会让所有 Pod 永远 Pending。
**负向对照已验**:uncordon 后调度器正常把 Pod 绑上并 Running;新建 Pod 走完整路径正常;
平台自己 namespace 的 Pod 不受影响。

配置在 `config/policy/tenant-deny-binding.yaml`。

⭐ **另一条路是 RBAC**:租户的 `*` on `*`(`controller/rbac.go:270`)才是准入口 ——
`kubectl auth can-i create pods/binding` 对租户主体返回 **yes**。
按 §8.0 判据收窄它属于 **kubezoo**。但那个通配是**有意的**(为覆盖租户 CRD),
而 RBAC **没有 deny 语义**,收窄就得改成枚举 —— 会丢掉 CRD 覆盖。
⇒ **VAP 是更合适的形态**,RBAC 这条列为备选。

## S. ⭐ kubezoo 侧的写路径拦截不成立 —— 租户 Pod 直连上游 ⛔ 实测坐实

**起因**:Kyverno 拦不住 `pods/binding`(§R⑤),那"让 kubezoo 来拦"行不行?
**答案:不行,而且原因是结构性的。**

### 租户有两类客户端,只有一类经过 kubezoo

| 客户端 | 路径 | kubezoo 拦得住 |
|---|---|---|
| 租户本人的 `kubectl` | 证书由 kubezoo CA 签,只能打 kubezoo | ✅ |
| 租户的 **Pod**(ServiceAccount token) | 直连上游 apiserver | ⛔ **完全不经过** |

kubezoo 自己的启动参数就是硬证据 —— 它**用上游的 SA 密钥签发 token**:

```
--service-account-key-file=$PKI/upstream/sa.pub
--service-account-signing-key-file=$PKI/upstream/sa.key
```

### 实测

**① 租户能给自己的 SA 授全权。** 租户在自己 namespace 是 `*` on `*`,
按 RBAC"不能授出自己没有的权限"这条,他**满足**条件:

```
租户建 Role sa-all (*/*/*) + RoleBinding 给 default SA   → 都成功
上游判定 system:serviceaccount:111111-default:default
  create pods/binding → yes
```

**② Pod 里看到的 API 端点是上游,不是 kubezoo。**

```
KUBERNETES_SERVICE_HOST=10.96.0.1     # 上游 kubernetes service 的 ClusterIP
上游 SelfSubjectReview 认出的身份 = system:serviceaccount:111111-default:default
```

**③ 从 Pod 内直连上游 POST binding —— 成功。**

```
HTTP 201
victim: node=kz-audit3-worker2        # 绑到了另一个租户的节点
```

**kubezoo 一行代码都没被执行。**

**④ 装上 VAP 后,同一条路被拒。**

```
HTTP 422
ValidatingAdmissionPolicy 'tenant-deny-binding' denied request
victim2: node=''
```

### ⭐ 由此补全 §8.0 判据缺的那一半

判据原来只说"读路径 ⇒ kubezoo"。这次证明它缺了对称的一半:

> **写路径的强制不能放在 kubezoo** —— 租户的工作负载直连上游,绕开它。
> Kyverno 做不到的写路径约束,退路是 **VAP/MAP 或 RBAC**(都在上游),**不是 kubezoo**。

⚠️ 这**不否定** kubezoo 在写路径上做的改写(前缀化等):那些保护的是**寻址**,
而直连上游的 SA 本来就只能在自己那个已加前缀的 namespace 里活动,由上游 RBAC 兜着。
两回事。

⇒ kubezoo 侧再加一道 binding 拦截,只是给 `kubectl` 用户的**减速带**,不是边界。**不做。**

## T. `Frozen` 停机拦不住租户预置的 ServiceAccount ⛔ 坐实 → ✅ **已修并复测**(#90)

#90 里我特意没标完成的那条,现在量出来了 —— 而且**比原先说的更严重**。

原先的说法是"控制面冻结管不到容器里已在跑的代码"。实测发现的不是这个:
**租户保留了一份完整可用的 API 凭据。**

冻结租户 111111 之后:

| | 结果 |
|---|---|
| 租户本人 `kubectl`(经 kubezoo) | ✅ **Forbidden** —— `tenant 111111 is suspended and is frozen` |
| 上游 `kubezoo:tenant-admin` RoleBinding | ✅ **已撤销** |
| 租户**自建**的 `sa-all` RoleBinding | ⛔ **原样还在** |
| 租户 Pod 用 SA 直连上游 `GET pods` | ⛔ **HTTP 200** |
| 租户 Pod 用 SA 直连上游 `CREATE configmap` | ⛔ **HTTP 201**,对象真的写进去了 |

⇒ **`Frozen` 冻的是租户的 kubectl,不是租户。** 预置一个 Pod 不需要多聪明 ——
它只要有个 token。⚠️ **所以 `Frozen` 更不能单独当取证冻结用**,这条比原先的表述硬得多。

### ✅ 修法与复测

两半:kubezoo 控制器在冻结时给租户每个 namespace 打 `kubezoo.io/frozen`
(`pkg/controller/rbac.go` 的 `markFrozen`),上游一条 VAP
(`config/policy/tenant-frozen-deny-writes.yaml`)见到这个标签就**拒绝本租户身份的写**。
标签是必需的:**上游看不到 Tenant 对象**,这是前门告诉上游"哪些 namespace 冻着"的唯一途径。

⭐ **表达式写成"放行不属于本租户的身份",不是"拒绝租户"** —— 冻结期间平台自己
仍要能动这些对象。判据是"请求者是不是本租户 namespace 里的 ServiceAccount":

```
system:serviceaccount:111111-default:default → 前缀 111111 == 目标 ns 前缀 ⇒ 拒
system:serviceaccount:kube-system:...        → kube != 111111              ⇒ 放行
system:node:... / system:kube-controller-manager(非 SA)                    ⇒ 放行
```

**⚠️ 只拦写不拦读**:冻结的承诺是"什么都不删、负载照常跑";读不销毁证据,
而拦掉读会让还在跑的负载直接崩。前门对租户 kubectl 连读也拒 —— 那是针对**操作人**的,
和这里不是一回事。

### 复测(9 条,含正向与负向对照)

| | 结果 |
|---|---|
| ① **正向对照**:冻结前,租户 SA 直连上游 | `GET 200` / `CREATE 201` / `DELETE 200` |
| ② 冻结后打标签 | **4 / 4** 个租户 namespace 全部打上 |
| ③ 冻结后,同一个 SA 同一条路 | **`CREATE 422` / `DELETE 422`**;`GET 200`(设计如此) |
| ④ 平台自己写冻结 ns | ✅ `configmap/platform-write created` |
| ⑤ **控制器链路**:平台在冻结 ns 里建 Deployment | ✅ ReplicaSet 1 / Pod 1 —— controller-manager 照常收敛 |
| ⑥ kubelet 仍在更新状态 | ✅ probe `Running`,restarts 0 |
| ⑦ 租户本人经 kubezoo(前门) | ✅ Forbidden,`is frozen` |
| ⑧ **解冻** | 标签 0 个;前门恢复;SA `CREATE 201` —— 两层一致 |
| ⑨ 非租户 namespace | ✅ kyverno 4 个 Pod 全 Running |

⑤ 是这条里最容易做错的:一条"拒绝一切写"的规则会把 controller-manager 一起拦掉,
而症状是**租户的 Deployment 永远不出 Pod**,不会有任何报错指向策略。

守卫测试 `pkg/controller/rbac_frozen_test.go`,**已验证摘掉 `markFrozen` 会变红**。

## U. 策略验证套件 `hack/lab/verify.sh` —— 以及它第一次跑就抓到的两件事

**为什么要有它**:本项目被"`READY=True` / 绿 / 装上了"骗过**四次**。
套件的每一条断言都是**提交一个必须被拒的东西,然后看它被谁拒**。
`hack/lab/verify.sh`,21 条,含正向对照;**已验证摘掉策略会红**(摘两条 → 4 条 FAIL,
且不误伤其余)。

### ⛔ 第一次跑就抓到:我自己引入的 VAP 打死了 Kyverno

`tenant-frozen-deny-writes`(commit `6d1baa9` 引入)让 **Kyverno 无法注册自己的 webhook**:

```
validatingwebhookconfigurations "kyverno-resource-validating-webhook-cfg" is forbidden:
ValidatingAdmissionPolicy 'tenant-frozen-deny-writes' denied request: expression '...'
```

**根因**:`matchResources.namespaceSelector` 对**集群级资源根本不过滤** ——
k8s 的语义是"cluster-scoped 资源永不跳过策略"。于是这条规则套到了
**全集群每一次集群级写入**上;那时 `request.namespace` 是空串,
`split('-')[0]` 越界让 CEL 表达式报错,而 `failurePolicy: Fail` 下**表达式报错 == 拒绝**。

**后果链**:Kyverno 注册不了 webhook → 三条策略永远不就绪 →
**validate webhook 只注册了 `daemonsets`,`pods` 根本没注册** →
租户的 `hostNetwork` / `hostPID` / `hostIPC` / `hostPath` / `spec.nodeName` **全部放行**。

⚠️ **而这一切的外在症状只是 `kubectl get clusterpolicy` 显示 `READY=<none>`** ——
看着像"还在同步中"。`up.sh` 早就把这行打在屏幕上了,**我看见了没反应**。

**修**:`matchConstraints.resourceRules` 加 `scope: Namespaced`,
再给表达式加一个 `request.namespace == ''` 的兜底。
`up.sh` 现在会**等待并在策略不就绪时直接失败**,而不是打印一行 `<none>` 就往下走。

> **教训:VAP 的 `namespaceSelector` 不是范围限定,`scope: Namespaced` 才是。**
> 而且 `failurePolicy: Fail` 下,**CEL 表达式出错等于拒绝** —— 一个越界就是全集群故障。

### ⚠️ 第二件:in-tree 的 RuntimeClass 准入跑在 mutating webhook 之前

租户写一个**不存在的** `runtimeClassName`,Pod 被直接拒:

```
pods "placed" is forbidden: pod rejected: RuntimeClass "kata" not found
```

Kyverno 的擦除规则**根本没机会跑**。这不是安全问题(租户只是自伤),
但意味着:**擦除只对平台真实存在的 class 有效** —— 而那恰好就是真正的威胁
(租户写 `runc` 想跑出 kata 沙箱)。所以套件的 fixture 改成先建一个真实的
平台 RuntimeClass 再让租户引用它,否则测的是另一回事。

### ⭐ 写这套东西时,我自己又踩了三次同一个坑

都是"**读一个不存在的对象**",而空值看起来正好像"字段被策略擦掉了":

1. Pod 被拒 → jsonpath 返回空 → 报"nodeSelector 被替换成空",实际是**对象不存在**
2. 等待条件写成"namespace 存在" —— **Terminating 的也算存在**,于是在正在销毁的
   namespace 里建 Pod,报 `because it is being terminated`
3. fixture 失败后仍继续断言,给出**假通过**("runtimeClassName stripped" 读的是空)

⇒ 套件现在:先确认对象存在再读字段;等 namespace `Active` 而不是"存在";
fixture 失败就**跳过**依赖它的断言,而不是让它们报绿。

## V. 拒绝消息泄漏扫描 —— 主结论干净,揪出两件别的

**做法**:以租户身份触发每一条策略的拒绝,逐条看**租户实际看到的字符串**,
在里面找上游痕迹(`111111-` 前缀、别的租户、平台 namespace)。

### 主结论:✅ 没有上游前缀泄漏

| 路径 | 结果 |
|---|---|
| 全部 7 条策略的拒绝消息 | ✅ **一条都不含 `111111-`** —— `TrimTenantIDFromError` 有效 |
| 按名字读**别的租户**的 CRD | ✅ `not found` —— **没有存在性预言** |
| 读 Node | ✅ `not found` |
| 跨租户 namespace | ✅ 回显的是租户自己敲的字符串 |

⚠️ **方法学**:第一遍 grep 报了四条"泄漏",逐条核实后**三条是租户自己的输入被回显**
(他自己敲的 `222222-default`、`private.example`)。
**判据是"这条信息是不是租户本来就知道的"**,不是"字符串里有没有出现别的租户"。

### ⛔ 真泄漏:webhook 名字暴露了平台用什么策略引擎

```
admission webhook "validate.kyverno.svc-fail" denied the request: ...
```

这句是 **apiserver 加的**,webhook 名字就是跑它的 service —— 等于告诉租户
"平台用 Kyverno,在 kyverno namespace"。知道是 Kyverno 就知道该试哪些绕过
(比如 `forceFailurePolicyIgnore`、版本 CVE),而这半句**对租户毫无用处** ——
他能用的是后半截策略消息。

**已修**:`TrimTenantIDFromStatus` 里加一条正则,擦掉
`admission webhook "..." denied the request: ` 这个前缀,保留后面全部。
按 §8.0 判据这是**读路径 ⇒ kubezoo 的**,而且和已有的前缀擦除是同一个机制、同一个位置。
守卫测试 `TestPlatformDetailIsRemovedFromMessages`,**已验证去掉实现会红**,
且断言"策略名/规则名/原因必须留下" —— 否则这笔交易不划算。

现场复测,租户现在看到:

```
resource Pod/default/z2 was blocked due to the following policies
tenant-scheduling: deny-nodename: 'spec.nodeName is not available to tenants: ...
```

## W. `ownerReference` 把内部前缀漏给租户 ⛔ 坐实并已修

扫消息时顺手查 system CRD 那段 `TODO: temporary fix`(FAQ 说"未实现"),
读码推出一个 bug,**实测坐实**。

`ownerReferenceTransformer.Backward` 传的是 `isTenantObject=true`,
于是**给已经带前缀的组再加一次前缀**:

```
上游存的      111111-example.com
Backward 查   111111-111111-example.com   → 匹配不上
落进 system-crd 分支 → Trim 一次 → 111111-example.com → 匹配上
                     → 但返回 customResourceGroup=false
                     → apiVersion 不被去前缀
```

**实测**:租户读回自己的 ConfigMap,`ownerReferences[0].apiVersion` = `111111-example.com/v1`。
不只是难看 —— 租户导出对象再 apply 回去会被**二次前缀**。

### 修法

- `Backward` 改传 `false`(上游来的组本来就带前缀,不该再加)
- `CheckGroupKindFunc` 里那段"system crd"分支**改成只在读方向生效**:
  平台控制器可能往租户对象上盖一个平台 CRD 的引用,租户得读得回来;
  但**写方向不能接受** —— 那既与 FAQ 的"未实现共享"矛盾,
  又是一个"这个 CRD 在上游存不存在"的预言机

守卫测试 `TestTenantCRDResolvesInBothDirections` / `TestPlatformCRDResolvesOnlyOutbound`,
**已验证退回旧行为两条都会红**。

### ⭐ 关于 system CRD 共享的产品建议:**先不实现**

- **今天没有消费者**。第一个候选是 kubetron 的网络 CRD
- 难点**不在 kubezoo**:kubezoo 只能决定"谁看得见"。真正的问题是平台 operator
  用平台凭据去调谐**租户写的 spec**,变成 confused deputy ——
  它按名字读 Secret 吗?引用 StorageClass 吗?按租户给的 spec 建 Pod 吗?
  **这个审查必须针对具体 operator 做,没法抽象地做**
- 集群级的共享 CR **没有 RBAC 兜底**(`resourceNames` 是精确匹配),
  与审计里已记的那条同一个坑

⇒ **重新评估的触发条件:出现第一个真实消费者,且能点名要审查哪个 operator。**

## X. ⛔⛔ 租户自己装 operator —— 实测**走不通**,而且我上一轮的说法是错的

**起因**:我说过"生态里其余的 operator,答案是租户自己装,kubezoo 天然支持"。
拿 cert-manager 的官方 chart 实测,**这个说法不成立**。

### 三个坎,前两个卡死 helm,第三个卡死 operator 本身

**① `helm --create-namespace` 不建 namespace。**

```
helm install ... -n freshns --create-namespace
→ 租户视角 freshns:      NotFound
→ 上游 111111-freshns:   NotFound
```

两次不同的 namespace 名都复现。手工 `kubectl create ns` 则正常(1 秒内可写,
所以**不是 RoleBinding 收敛竞态**)。原因未追。

**② 租户建不了任何 ClusterRole。**

```
clusterroles "cert-manager-cainjector" is forbidden: user "admin" is attempting to
grant RBAC permissions not currently held:
  {APIGroups:[""], Resources:["events"], Verbs:["get" "create" "update" "patch"]}
  {APIGroups:[""], Resources:["secrets"], Verbs:["get" "list" "watch"]}
  ...
```

⭐ **注意漏出的不只是 CRD 组,连 `events` / `secrets` 这种核心资源都不行** ——
因为租户的权限是**逐 namespace 授予**的,**集群级什么都不持有**,
而 RBAC 的提权防护要求"你不能授出你没有的"。

⇒ **任何带 ClusterRole 的 chart 都装不上**,而那是绝大多数 operator,以及很多普通 chart。
cert-manager 一次报 21 个错。

**③ ⭐⭐ 最要命的:operator 在 Pod 里看不见自己的 CRD。**

租户的 CRD 组会被加前缀,而 **operator 的 Pod 直连上游、不经过 kubezoo**
(§S 已实测),于是它按代码里写死的组名去查,查不到:

```
# 从租户 Pod 内,用它的 ServiceAccount 直连上游
GET /apis/example.com/v1          →  HTTP 404      ← operator 代码里写的
GET /apis/111111-example.com/v1   →  HTTP 200      ← 上游实际存的
```

**租户视角看到的是 `example.com`,operator 视角看到的是 `111111-example.com`。**
这两个视角的差异正是 kubezoo 的立身之本,但 operator 站在错误的那一边。

### 结论:我上一轮的建议要翻过来

我原来说"per-operator 审查只针对平台托管的那几个,生态里其余的租户自己装" ——
**后半句不成立**。租户自装被两条**互相独立**的结构性问题卡住:
② 装不上(ClusterRole),③ 装上也跑不起来(组名错位)。

要让它通,需要同时满足:

- **把 operator 指向 kubezoo 而不是集群内的上游端点**(给它挂一份租户 kubeconfig)——
  这样它看到的就是去前缀的视图。⚠️ 本轮 lab 里 kubezoo 绑在 `127.0.0.1`,
  Pod 够不着,**这条路只在架构上成立,没实测**
- **operator 支持 namespace 级运行、只用 Role 不用 ClusterRole** ——
  cert-manager 不支持;支持的是少数

⇒ **能自装的是生态里的一个小子集,不是"其余全部"。**
这反过来让**平台托管形态更重要**,不是更不重要 —— 跟我上一轮的结论方向相反。

### ⚠️ 顺带一条运维事实

本轮 lab 没建节点池,于是 `tenant-placement` 注入的 `nodeSelector` 指向一个不存在的池子,
**租户的每一个 Pod 都卡在 Pending**。这是 fail-closed,是对的 ——
但**症状是"Pod 不调度",没有任何东西指向策略**。运维手册已记。

## Y. 把租户负载指向 kubezoo ✅ **已打通**(SA token 认证缺口已修)

**要验的**:让 operator(以及所有租户 Pod)用自己的 ServiceAccount token 打 **kubezoo**
而不是上游,这样它看到的就是**去前缀的视图**,§X 那三个坎里最要命的第 ③ 条就消失,
而且**每租户不同 operator 版本天然成立**(组名前缀让同名 CRD 不撞车)。

### 已经成立的那一半

`pkg/filters/tenant.go` 的 `WithTenantInfo` **已经支持从 SA 推租户**:

```go
namespace, _, err := serviceaccount.SplitUsername(user.GetName())
tenantID, err := util.GetTenantIDFromNamespace(namespace)
```

`system:serviceaccount:111111-default:default` → 租户 `111111`。设计上就是为这条路准备的。

### ⛔ 认证这一半原先是断的 —— 已修

lab 里把 kubezoo 的 `--service-account-issuer` / `--api-audiences` 对齐上游后,
从租户 Pod 内实测仍然 **401**:

```
invalid bearer token, Internal error occurred: authentication failed unexpectedly
```

**根因**:kubezoo **从未设置 `ServiceAccountTokenGetter`** ——
全仓 grep 只在生成的 openapi 里出现过这个名字。上游 kube-apiserver 是设的
(`pkg/kubeapiserver/options/authentication.go:712/719`,从 informer 或 client 构造)。
没有它,**绑定型 token 无法验证**,而 1.21 之后 kubelet 投射的全是绑定型。
`WithTenantInfo` 应该是 1.24 那个 fork 时代留下的 —— 那时非绑定 token 还在,这条路是通的。

### ✅ 修法与实测

`cmd/kubezoo/app/tokengetter.go`:用 kubezoo **已有的**上游客户端
(`ProxyConfig.typedClientSet`)实现 `ServiceAccountTokenGetter` 的六个方法,
在 `applyAuthenticationOptions` 里接上。

⚠️ 不能直接用上游的 `serviceaccountcontroller.NewGetterFromClient` ——
它无条件解引用 lister,而 kubezoo 不跑对上游的 informer。
⚠️ getter 必须走**原生上游客户端**,不能走 kubezoo 自己的转换层:
token 里的名字(`111111-default` / `default`)本来就是上游名字,再前缀一次就查不到了。

**实测(从租户 Pod 内,只用它自己的 ServiceAccount token)**:

| | 打上游 | 打 kubezoo |
|---|---|---|
| SA token 认证 | — | ✅ **200**(修前 401) |
| `/apis/example.com/v1` | ⛔ 404 | ✅ **200**,返回 `widgets` |
| `/apis/111111-example.com/v1` | 200 | 200 |
| **建一个 Widget** | — | ✅ **HTTP 201** |
| 上游实际落地 | — | ✅ `widgets.111111-example.com` in `111111-default` |

⇒ **operator 用自己的 SA token 打 kubezoo,就能按代码里写死的组名发现并读写自己的 CR。**

守卫在 `hack/lab/verify.sh`(第 22、23 条),**已验证摘掉接线会红,且只红这两条**。

⚠️ **配置前提**:kubezoo 的 `--service-account-issuer` / `--api-audiences`
**必须等于上游的**(它本来就用上游的 SA 密钥签名)。`up.sh` 现在从上游 apiserver 读出来,
不再硬编码 `foo`。

### ⭐ 方法学:第一次"红测"是无效的

摘掉接线跑套件确实红了 —— 但我用的重启脚本是 scratchpad 里的旧版本,
还带着 `--service-account-issuer=foo`。**那次的 401 可能来自 issuer 不匹配,而不是我摘掉的东西。**
用正确的 flag 重做,才确认"只摘 getter ⇒ 恰好这两条红"。
**判据:红测里除了被测的那一处,其它条件必须和绿测完全一致。**

### 这条路一旦通了,顺带解决的

- §X③ operator 看不见自己的 CRD 组 —— 消失
- **每租户不同 operator 版本** —— 天然成立(这是共享托管形态给不了的)
- §T `Frozen` 被租户预置 SA 绕过 —— 从根上消失
- §Q/§S `pods/binding` 绕过 —— 从根上消失

### ⚠️ 但还有三件没验、且不会自动消失

1. **ClusterRole 那道坎照旧**(§X②)。提权检查在上游按租户真实权限做,走不走 kubezoo 一样。
   除非改成"给租户在自己前缀的组上授集群级权限" —— 单独的设计问题
2. **kubezoo 要吃下全部租户负载的 API 流量**,流量模型的根本改变,直接撞 #84/#85
3. **透明重定向**(用 mutate 策略注入 `KUBERNETES_SERVICE_HOST`,podspec env 压过 kubelet 注入的)
   —— 架构上成立(kubegateway 那边已验证过同一机制),**这里没测**

## Z. ⭐ 注入 `KUBERNETES_SERVICE_HOST` 之后,四个缺口逐条重测

§Y 打通了"租户负载能用自己的 SA token 打 kubezoo",这一节验的是**让它们真的都走 kubezoo**
之后会发生什么。手段是一条 mutate 策略(`config/policy/tenant-api-endpoint.yaml`):
给租户 Pod 的每个容器注入 `KUBERNETES_SERVICE_HOST/PORT` 指向 kubezoo ——
**podspec 里同名 env 压过 kubelet 注入的那个**,workload 零改动。
`client-go` 的 `InClusterConfig` 正是读这两个变量(`rest/config.go:551`)。

### ⚠️ 先解掉一个 TLS 前提(否则整条不成立)

Pod 用 `/var/run/secrets/.../ca.crt` 校验服务端,而那是**上游集群的 CA**。
kubezoo 用自签证书的话,**所有 in-cluster 客户端一律 TLS 失败**。
lab 里的做法:**用上游集群 CA 给 kubezoo 签一张服务证书**(SAN 覆盖 Pod 用的地址)。
之后 Pod **不加 `-k`** 也能校验通过 —— 已实测。

⇒ 这是部署前提,不是可选项:**kubezoo 的服务证书必须由 Pod 信任的那个 CA 签发。**

### 逐条重测

| 缺口 | 注入前 | 注入后 |
|---|---|---|
| **§X③ operator 看不见自己的 CRD 组** | `/apis/example.com/v1` → **404** | ✅ **200**,且能写(Widget 创建成功) |
| **每租户不同 operator 版本** | 共享形态给不了 | ✅ **成立**(见下) |
| **§T `Frozen` 被租户预置 SA 绕过** | `CREATE` → **201**(照常写) | ✅ **403**;binding 报 `tenant 111111 is suspended and is frozen` |
| **§Q/§S `pods/binding` 绕过** | 直连上游 **HTTP 201**,绑到别的租户节点 | ✅ 被 `tenant-deny-binding` VAP 拒 |

### ⭐ 每租户不同版本 —— 实测

两个租户各自建**同名组** `example.com` 的 CRD,版本不同:

```
上游:  widgets.111111-example.com   group=111111-example.com   v1
       widgets.222222-example.com   group=222222-example.com   v2

租户 111111 的 Pod:  /apis/example.com/v1 → 200    /apis/example.com/v2 → 404
租户 222222 的 Pod:  /apis/example.com/v1 → 404    /apis/example.com/v2 → 200
```

**同一个组名,各看各的版本。** 这正是"平台装一个共享 operator"结构上给不了的
(一个 CRD 只有一个 storage version、一个 controller),
而 kubezoo 的组名前缀在这里从"障碍"变成了"能力"。

### ⛔ 没有被解决的

**§X② 租户仍然建不了 ClusterRole。** 实测仍是
`is forbidden: ... attempting to grant RBAC permissions not currently held`。
提权检查在**上游**按租户真实权限做,走不走 kubezoo 都一样。
⇒ **带 ClusterRole 的 chart 仍然装不上**;能自装的仍限于"能 namespace 级运行、只用 Role"的 operator。
要放开需要单独设计(例如给租户在**自己前缀的组**上授集群级权限),没做。

### ⚠️ 随之而来的代价

**kubezoo 现在在租户全部工作负载的 API 路径上。** 这是流量模型的根本改变,
直接变成 #84/#85 的核心议题。用户已确认接受,并计划用 kubegateway 挡在前面做精准管控。

## AA. `helm --create-namespace` 为什么不工作 —— 抓到请求了

**不是 kubezoo 的 namespace 创建有问题**(kubectl 三种写法都正常:`create ns`、
`create -f` 的 Namespace 清单、带 labels 的清单,上游全部落地)。
**也不是 helm 的 bug**(同一条命令打上游集群成功)。

把 kubezoo 开到 `-v=6`,helm 打过来的**全部**请求是:

```
GET /api/v1/namespaces/minins4/secrets          ← 查历史 release
GET /api  /api/v1  /openapi/v3                  ← discovery
GET /api/v1/namespaces/minins4/configmaps/mini  ← 检查资源是否已存在 → Forbidden,中止
```

**根本没有 `POST /api/v1/namespaces`。** helm 一次都没尝试建 namespace。

### 根因:顺序 + 错误码语义

helm **先**检查 chart 里的资源是否已存在,**再**建 namespace。而:

| | 在不存在的 namespace 里 `GET` 一个对象 |
|---|---|
| 上游、cluster-admin | **NotFound** → helm 继续 → 建 namespace → 装成功 |
| kubezoo、租户 | **Forbidden**(那个 namespace 没有 RoleBinding)→ helm 当致命错误中止 |

⇒ 差别在**错误码**,不在能力。

### ✅ 已修:Forbidden → NotFound

`pkg/proxy/proxy.go` 的 `shapeError`:租户 **Get** 一个对象时若拿到 Forbidden,
且目标 namespace **对该租户确实不存在**,就改写成 NotFound。

- 与租户视角一致:那个 namespace 在他的世界里就是不存在,里面的对象自然也不存在
- 按 §8.0 判据属于**读路径 ⇒ kubezoo 的**,与 `TrimTenantIDFromError` 同一处机制
- 只在错误路径多一次查询,热路径无开销
- ⚠️ **窄范围**:只对 Get、且只在确认 namespace 不存在后改写。List 不动 ——
  上游那里是返回空列表而不是 NotFound,对齐它需要**合成响应**而不是改写错误
- 守卫:`verify.sh` 两条(缺失 namespace 读作 NotFound / **已存在 namespace 上的真拒绝仍是 Forbidden**)

### ⛔ 但还有第二个坎:RBAC 授权器的缓存延迟

修完之后 helm **确实会建 namespace 了**,但紧接着写自己的 release secret 时仍被拒:

```
第一次 helm install --create-namespace → 建出 ns,在 secret 上失败
立刻重试(ns 已存在)                  → STATUS: deployed
```

量出来的窗口:

| | |
|---|---|
| 建 ns 后 RoleBinding **出现** | **169 ms** |
| 建 ns 后**真的能写** | **312 ms** |

中间那 ~143ms 是**上游授权器自己的缓存延迟** —— RoleBinding 已经在 etcd 里了,
授权器还没看到。

### ⚠️ 试过让 kubezoo 在建 namespace 时同步下发 RoleBinding —— 不成立,已撤

想法是在代理的 Create 成功后立刻建 RoleBinding。**实测直接失败**:

```
could not bind new namespace 111111-t1 for tenant 111111:
rolebindings.rbac.authorization.k8s.io is forbidden:
User "111111-admin" cannot create resource "rolebindings" ... in the namespace "111111-t1"
```

⭐ **是循环的**:`pkg/dynamic` 给每个请求都带上租户的 impersonation 头,
所以这个创建请求也是**以租户身份**发出的 —— 而租户在这个刚建的 namespace 里
恰恰还没有权限,正是要修的那件事。

即便改用 kubezoo 自己的凭据绕过这一点,**上面那 143ms 授权器缓存延迟仍在**,
helm 照样输。⇒ **这个竞态关不掉**,代码已撤回。

### 结论

`helm --create-namespace` 从"**永远失败,namespace 都建不出来**"变成
"**失败一次,重试即可**"。对 helm 这是正常工作流;要一次成功仍需先手工建 namespace。

⚠️ 这条限制影响**所有"建 namespace 后立刻写入"的工具**(helm / kustomize /
`kubectl apply -f 目录`),而且**不是 kubezoo 独有** —— 任何依赖 per-namespace
RoleBinding 的多租户模型都撞同一堵墙。

## AB. 租户可以给自己的 CRD 授权了(ClusterRole P0 的第一半)

**问题**(审计 §X②):租户建不了任何 ClusterRole。RBAC 的提权防护要求
"不能授出你没有的",而租户的权限是**逐 namespace 授的、集群级零持有** ——
连引用它**自己刚建的 CRD** 都不行。cert-manager 一次报 21 个错。

⚠️ 不能靠给 `escalate`/`bind` 解决:`rbac.go` 里有实测记录,给了之后
租户能建 `*` on `*` 的 ClusterRole 绑给自己,**摸到别的租户的 secret 和 kube-system**。

### ✅ 已修:把租户自己的 CRD 组授给它(集群级)

`pkg/controller/rbac.go` 的 `ownCustomResourceRules`:租户的 ClusterRole 里,
按它自己的 CRD 组逐条加 `apiGroups: [<tid>-<组>], resources: ["*"], verbs: ["*"]`。

**为什么安全**:组名本身带租户前缀。`111111-cert-manager.io` 里**只可能有租户 111111 的对象**
(kubezoo 在写入时给 CRD 组加前缀),所以"集群级"在这里等于"我自己的全部"。

**为什么要枚举**:RBAC 没有前缀 —— apiGroup 只能是字面量或 `*`,`111111-*` 两者都不是。
所以每轮从租户的 CRD 重新构造。

### ⚠️ 顺带补上的:CRD 变化要能触发同步

第一版只在租户事件上同步 ⇒ **租户建完 CRD 后,得等到下一次 resync(最坏 10 分钟)
才能给它写 ClusterRole**,实测确认(戳一下租户才成功)。
加了 CRD informer,按**组名前缀**推出租户并入队(平台自己的 CRD 没有前缀,不归任何人)。
复测:**2 秒内自动生效,不用戳**。

### 边界复测(都实测)

| 尝试 | 结果 |
|---|---|
| 引用自己的 CRD 组 | ✅ created |
| 引用 `secrets`(共享资源) | ⛔ Forbidden |
| 引用 `pods`(核心资源) | ⛔ Forbidden |
| 222222 引用 111111 的组(租户视角名) | ⛔ Forbidden |
| 222222 直接写 `111111-two.example`(上游真名) | ⛔ Forbidden,**上游零残留** |

守卫 `verify.sh` 三条,**已验证摘掉授权会红**(恰好那一条)。

### ⛔ 还剩第二半:共享资源

cert-manager 这类 operator 的 ClusterRole 还要 `events` / `secrets` 等**共享资源**的集群级权限。
那个**不能给** —— 给了就是跨租户。

正确的模型是:**租户的"集群"就是它那组 namespace** ⇒
租户建的 ClusterRole/ClusterRoleBinding 应当被**投影**成每个租户 namespace 里的 Role/RoleBinding。
⭐ 关键观察:**ClusterRole 对象本身不危险,不绑定就什么都不授** —— 危险的是 ClusterRoleBinding。
所以约束点应该放在**绑定**上,而不是禁止建角色。

三个待解问题(未做):读回(helm 建完会 `get` 它)、新 namespace 的补投影、更新/删除的传播。

## AC. ClusterRole 第二半:两个前提成立,一条捷径是陷阱(实测)

**起因**:`events` / `secrets` 都是**命名空间级**资源,租户在自己的 namespace 里
本来就有它们的全部权限。所以第二半失败的原因不是"这些资源不该给",而是
**提权检查问错了问题** —— 它问"你在**集群级**持有吗",而在 kubezoo 的模型里
有意义的问题是"你在**你所有的 namespace** 里持有吗",后者答案是**是**。

### ✅ 两个前提,都实测成立

| | 结果 |
|---|---|
| 租户在自己 ns 里用 **RoleBinding** 引用一个 `*` on `*` 的 ClusterRole | ✅ **created** —— 检查是按"在该 ns 里持有什么"做的,而租户在那里就是 `*` |
| 租户建 **ClusterRoleBinding** 把它绑成集群级 | ⛔ **Forbidden** —— 而且是**上游 RBAC 独立拦的,不依赖 kubezoo** |

⇒ 所以缺的只是**建 ClusterRole 对象本身**;下游的约束**已经是对的**,
而且第二道是独立于 kubezoo 的。

### ⛔ 但"只给 escalate 不给 bind"这条捷径是**完全逃逸**

看起来很诱人:`bind` 才是危险的那个动词,只给 `escalate` 应该只能建角色、绑不了。
**实测:租户直接拿到 cluster-admin。**

机制很具体:

```
租户 apply 一个叫 cluster-admin 的 ClusterRole
  → kubezoo 前缀化成 111111-cluster-admin
  → 而那正是控制器给它建的、已经用 ClusterRoleBinding 集群级绑定的那个角色
  → escalate 让它可以写入自己不持有的规则
  → 覆盖成 ['*'] ['*'] ['*']
```

上游对租户主体的判定,攻击后:

```
can-i get secrets -n kube-system      → yes
can-i list nodes                      → yes
can-i get secrets -n 222222-default   → yes    ← 别的租户
can-i create clusterrolebindings      → yes
```

**根因不是 escalate 本身,是命名撞车**:kubezoo 给租户的保留对象叫 `<tid>-cluster-admin`,
而**租户只要把自己的 ClusterRole 命名为 `cluster-admin` 就能按名字够到它**。
⭐ **kubezoo 自己的控制对象和租户的对象活在同一个名字空间里。**

### 不给 escalate 时,这条撞车还剩什么

- 租户**可以删掉** `111111-cluster-admin`(它有 `delete` 动词)—— 实测删成功。
  这是**自伤**(它自己失去集群级权限),控制器 **4 秒补回**
- 租户可以覆盖它,但只能写入自己已持有的规则 ⇒ 只能收窄,同样是自伤

⇒ 今天不是漏洞,但是**任何将来给这条路加权限的改动都会踩上它**。

⚠️ 顺带核实过一件容易误报的事:攻击后 `can-i list nodes → yes`,
收敛之后**仍然是 yes** —— 那是**设计本来就有的**(kubezoo 在读路径上藏 Node,
上游 RBAC 仍授 list),不是攻击残留。

### 第二半的设计(未实现)

kubezoo 用**自己的身份**替租户写 ClusterRole(上游 RBAC 表达不了"在我所有 namespace 里持有"),
下游靠已经验证过的两条约束兜住。

**必须带的守卫**(由上面那条撞车直接推出):

1. ⭐ **拒绝以特权身份写 kubezoo 自己的保留名字** —— 首先是 `<tid>-cluster-admin`。
   否则就是上面那条逃逸,只是换了个触发方式
2. 保留 ClusterRoleBinding 的拒绝(上游已拦,kubezoo 不要另开后门)
3. 部署要求要写清:kubezoo 的上游身份需要能写任意 ClusterRole(`escalate` 或 cluster-admin)——
   **这是一项真实的信任扩大**,必须显式记录而不是顺带获得

**三个待解**:读回(helm 建完会 `get` 它)、新 namespace 的补投影、更新/删除传播。

## AD. Lease 会不会漏出节点清单和平台指纹 ✅ 不会(五条探测)

**为什么专门测 Lease**:`kube-node-lease` 里**每个节点一个 Lease,名字就是节点名** ——
等于一份完整的节点清单(数量 + 名字 + 活性)。而 `kube-system` 和策略引擎 namespace 里的
Lease,持有者写的是 `kyverno-admission-controller-f849f45fd-c549g` 这种 ——
**正是我从拒绝消息里擦掉的那个"平台用什么策略引擎"指纹**(§V)。

上游真实存在的:

```
kube-node-lease   kz-audit3-control-plane   ← 节点名
kube-system       kube-controller-manager / kube-scheduler / apiserver-...
kyverno           kyverno / kyverno-background-controller
```

### 租户侧五条探测,全部封死

| 探测 | 结果 |
|---|---|
| `get leases -A` | `No resources found` |
| `get leases -n kube-node-lease` / `-n kube-system` | 空(前缀化成租户自己的那两个) |
| 裸路径 `/apis/coordination.k8s.io/v1/leases`(集群级) | `items: []` |
| 裸路径 `/apis/.../namespaces/kube-node-lease/leases` | `items: []`(同样被前缀化) |
| `describe lease <节点名> -n kube-node-lease` | **NotFound** |
| `--field-selector metadata.name=<节点名>` | `No resources found` |
| 直接写上游真名 `-n 111111-kube-node-lease` | **Forbidden**(被再前缀一次,够不着) |

⭐ **靠的不是任何 Lease 专属逻辑,就是 namespace 前缀。**
这也说明同一类"平台基础设施藏在命名空间级资源里"的东西(Endpoints、Events、
ConfigMap 里的 leader election 记录)都由同一个机制兜着。

⚠️ 注意 `-A` 这条**在扇出之前是 Forbidden**(集群级 LIST 无权限),现在是"正常返回且为空"——
从"被权限挡住"变成"真的查了自己的全部 namespace,确实没有"。**后者才是可依赖的。**

守卫 `verify.sh` 三条(跨 namespace 列举不触及平台 namespace / 按名字 NotFound / 裸路径落在租户自己的)。

## 尚未覆盖

诚实列出,不算做完:

- ~~`-A` / 全集群 LIST 现在对租户直接 Forbidden~~ ✅ **已解决(LIST 部分)**:
  改成逐 namespace 扇出后,`-A` 从 `Forbidden ... at the cluster scope` 变成正常返回 ——
  逐 namespace 读租户有权限,全集群读没有。实现 `pkg/proxy/fanout.go`,
  设计 `design-list-fanout-cn.md`,实测前后对照见架构文档 §7.2。
  `-A` 的 **watch** 也已改为多路复用(`pkg/proxy/watchmux.go`)。
  ⚠️ 仍未改的只有 **cluster 级资源**(全量 + 前缀过滤)—— **有意保留**:
  标签下推方案实测会让所有存量对象从租户视角消失,见 `design-list-fanout-cn.md` §6
- 跨租户 Ingress host/path 抢占:一旦两租户都接到平台 ingress 控制器上(见 I②),
  归属由控制器裁决,kubezoo 不参与 —— 未测,属 3.1 策略层
- 租户自建 webhook 的 `failurePolicy: Fail` 对平台组件的影响面(已限 Namespaced + 本租户 ns,
  但同租户内仍可自锁)—— 未测
