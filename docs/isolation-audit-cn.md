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

## P. 节点名从 `spec.nodeName` 漏给租户 ⚠️ 观察到,未修

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

### 归属与为什么先不改

按 §8.0 判据,这是**读路径 ⇒ kubezoo 的**,策略层结构上够不着(准入看不到响应)。
但**先别急着改**:

- 泄漏面还没摸全:`-o wide`、`status` 里、events、PV 的 `nodeAffinity`
- `-o wide` 显示 NODE 是 kubectl 的常规行为,一刀切藏掉会让租户排障体验碎掉

**"改写成假名" vs "接受这个泄漏并写进文档"是个待定决策**,
建议等 B1 的节点池方案定了一起处理(架构 §8.2.3)。

## 尚未覆盖

诚实列出,不算做完:

- **`-A` / 全集群 LIST 现在对租户直接 Forbidden**(#87 的副作用,实测)。
  这顺带堵掉了 TODO 1.2 说的"全量 LIST + 过滤"规模墙与 DoS 面,
  但也意味着 `kubectl get pods -A` 对租户**不可用** ⇒ 需按 TODO 1.2 改为
  "先取租户 namespace 列表,再逐 namespace scoped LIST 合并"
- 跨租户 Ingress host/path 抢占:一旦两租户都接到平台 ingress 控制器上(见 I②),
  归属由控制器裁决,kubezoo 不参与 —— 未测,属 3.1 策略层
- 租户自建 webhook 的 `failurePolicy: Fail` 对平台组件的影响面(已限 Namespaced + 本租户 ns,
  但同租户内仍可自锁)—— 未测
