# KAaaS 平台落地 TODO

本文件是 [`docs/kaaas-platform-architecture-cn.md`](docs/kaaas-platform-architecture-cn.md)
的任务分解。架构文档回答"为什么这样设计",本文件回答"还剩什么没做"。

## 使用约定

- **做完一项就勾掉**,并在条目末尾补上 commit 短 hash。
- **实现过程中如果发现架构文档写错了,或者最终采用了别的方案,先改架构文档,再回来
  改本文件的条目**(修正措辞、或直接删除已不适用的任务)。两份文档不允许长期不一致。
- 条目里的 `#NN` 是任务系统里的编号,细节在那里,本文件只保留判断所需的最小信息。
- ⚠️ 标记的是**做的时候容易踩或容易忘**的点,不是可选项。

## 阶段总览

| 阶段 | 内容 | 门槛 |
|---|---|---|
| **0** | 移植到 k8s 1.36.3 | 不通则后面全部无从谈起 |
| **1** | 安全基线 | 决定阶段 2 审计的严苛程度 |
| **2** | 隔离正确性审计 | 决定这套方案能不能对外 |
| **3** | 平台层(Kyverno / kubetron / kata) | B1 架构真正成立 |
| **4** | 规模与性能 | 产品天花板 |

---

## 阶段 0:移植到 k8s 1.36.3(#83 / #88)

基线 `v1.36.3`(stable,与 kubetron 的 `k8s v0.36` 对齐)。分支 `port/k8s-1.36`。

### 0.1 生成代码配方 ✅

- [x] 重建 `pkg/apis/openapi/zz_generated.openapi.go` 的生成配方(此前**无任何配方**) — `4fadaa5`
- [x] `hack/make-rules/codegen.sh` + `make codegen` / `make verify-codegen` — `4fadaa5`
- [x] 修 Makefile 的 `openap` 拼写与"bare list 替换默认集"两个 bug — `4fadaa5`

### 0.2 依赖抬升 ✅

- [x] staging 全部 `v0.36.3`、`k8s.io/kubernetes v1.36.3`、kube-openapi/utils/klog 对齐 — `58959f1`
- [x] controller-runtime `v0.24.1`、apiserver-runtime `v1.1.1`、structured-merge-diff `v4→v6` — `58959f1` `22e6133`
- [x] `go mod tidy` 通过 — `58959f1`

### 0.3 编译修复

- [x] `pkg/` 全部 + `cmd/clusterresourcequota` 编译通过 — `58959f1` `22e6133` `6ce59fc`
- [ ] **`cmd/kubezoo/app/customresource_handler.go`(5 个错误)** — **#88**,方法见下
- [ ] `cmd/kubezoo/app/server.go`(6 个错误):`Features.ApplyTo` 参数、`genericConfig.Version` 已移除、
      `storageFactoryConfig.Complete` 返回值数量
- [ ] `go build ./...` 全绿

### 0.4 CRD handler 重新 fork(#88)

它是上游同名文件的 fork,已与 1.36 **结构性脱节**。逐编译错误改会漏掉上游 900 多行演进
(编译过但行为停在 1.24),而这个文件是 **CRD 的租户隔离路径**。

- [ ] 按**三方合并**解 14 个冲突块(base=上游 v1.24.0 / ours=kubezoo fork / theirs=上游 v1.36.3)
      - 产物已在 `_output/refork/`(gitignore),复现命令在 #88
      - ⚠️ 先看两个大块:**#11 行 1273-1348(76 行)、#12 行 1413-1468(56 行)**
- [ ] 解冲突时保住 kubezoo 语义:CRD 名字/group 的租户前缀转换(`util.ConvertCRDNameToUpstream`)、
      `getTenantCRD`、文件末尾新增的 ~55 行
- [ ] 合并后确认这些**已解的改动没被回退**:fieldmanager→`apimachinery/util/managedfields`、
      `StaticOpenAPISpec` 换 `map[string]*spec.Schema`、openapi V3、
      `fieldpath.NewExcludeFilterSetMap`、ServerSideApply 门移除
- [ ] 对齐仍未解的 1.36 API:`customresource.NewStorage` 返回 2 值、`NewStrategy` 参数表
      (structural 由 map 变单个 + 末尾新增 `[]apiextensionsv1.SelectableField`)、
      `NewSchemaValidator` 收 `*JSONSchemaProps`、`GetRESTOptions` 加参数

### 0.5 收尾

- [ ] `apigroups.go`(1225 行)按 1.36 **全面核对**资源/版本清单
      (目前只删了编译报错的 autoscaling v2beta1/v2beta2 与 PodSecurityPolicy)
- [ ] `pkg/util/util.go` 的 `groupKindNamespaced` 表(60+ 条)按 1.36 更新
- [ ] `make codegen` 重新生成 + `make verify-codegen` 通过
- [ ] ⚠️⚠️ **`go test ./...`** —— 整个移植至今**唯一证据只有"能编译"**。重点验:
      - `NewQuotaConfigurationForAdmission(nil, nil)` 两个新参传 nil 是否安全
      - controller-runtime 事件处理器的泛型迁移
      - CRD handler 的 openapi V2→V3 改动
- [ ] 合并回 main

> ⚠️ **方法学**:"还剩 N 个错误"**不是可靠进度指标** —— 每修好一处就暴露下一处
> (server.go 曾 6→3→又 6)。已据此低估过一次工作量,排期时别按错误数估。

---

## 阶段 1:安全基线

### 1.1 per-namespace RBAC(#87)⭐ 最高优先

现状:每租户在上游被授予 `*` on `*`(`pkg/controller/controller.go:556`,绑定 `:614`),
kubezoo 自身 `--authorization-mode=AlwaysAllow` ⇒ **隔离完全依赖改写层,零兜底**。

- [ ] 租户创建 namespace 时,同步在该 namespace 内生成 RoleBinding,把权限限死在自己的 namespace
- [ ] 顺带**限制租户对自己 namespace 的 update 权限**(防摘除策略标签,见 3.1)
- [ ] 验收:两租户跨访问 namespaced 资源时**上游 RBAC 拒绝**(而非只靠改写层挡),每条带负向对照

> ⚠️ **硬边界**:RBAC 的 `resourceNames` 是**精确匹配**,无通配无前缀
> (`k8s/pkg/apis/rbac/v1/evaluation_helpers.go:86`)。
> Namespaced 资源(40+ 种)✅ 可兜底;**cluster-scoped(PV/StorageClass/Namespace/CRD… 20+ 种)
> ❌ 永远只能靠改写层**。
>
> **本项做完,#82 的性质从"必须零遗漏否则即越权"降为"找漏,漏了还有网"**,所以排在审计前。

### 1.2 已知缺陷修复

- [ ] **Node 对所有租户无条件可见** —— 删掉 `pkg/util/util.go:136-144` 那个为过 Conformance
      加的 TODO 分支。代价:Conformance 测试会挂,是取舍不是难题
- [ ] **`-A` 与 cluster-scoped 请求走"全量 + 过滤"** —— 改为:先取该租户的 namespace 列表,
      再逐 namespace 发 scoped LIST 合并。代价是请求放大,但数据量从全集群降到租户自身
      (同时解决 4.1 的规模墙与 DoS 面)
- [ ] **DaemonSet 未在代理层拒绝** —— FAQ 称限制,但 `apigroups.go` 正常注册代理。由 Kyverno 策略补(见 3.2)
- [ ] **system CRD 共享机制未实现** —— FAQ 描述了但全仓零命中。决定:实现,还是把 FAQ 改成实话?

### 1.3 三方对象契约 ⭐ 必须先定再实现

kubezoo / kubetron / Kyverno **都会往租户的 Pod 上写东西**,而**只有 kubezoo 在出站口能擦**。
租户 `kubectl get pod -o yaml` 会看到:被改写成 `exec` 的探针、Multus 注解、ShardLabel、
注入的 `runtimeClassName`、被清空的 `tolerations`、暴露平台节点名的 `nodeName`。
后果:GitOps 永久漂移、租户排障对不上、平台拓扑泄露。

- [ ] 约定一份"平台内部字段"清单,kubezoo 出站时统一擦除
- [ ] **被改写的字段必须可还原** —— kubetron 把原探针存进约定注解,kubezoo 出站还原。
      这是**跨项目接口,必须先定**,不能事后补
- [ ] 建立规矩:每新增一个会变异租户对象的平台组件,同步登记进该清单

> ⚠️ 越晚定,kubezoo 的 convert 层越会碎片化 —— 而那正是 1.1 说的"最不该复杂化"的地方。

---

## 阶段 2:隔离正确性审计(#82)

⚠️ 应在 **1.1 之后**做。

- [ ] **静态覆盖审计**:枚举 k8s 全部资源 × 全部含引用的字段,对照 `pkg/convert/` 找漏网。
      重点:ownerReference、objectReference、Service/Endpoints/EndpointSlice、PV↔PVC 绑定、
      RoleBinding 的 subjects、CRD group 前缀、webhook 的 clientConfig.service、Event 的
      involvedObject、`cross-object.go`。确认哪些资源落到了默认放行的 `default.go`/`nope.go`
- [ ] **双租户黑盒穿越测试**(真实 kubectl/client-go,优先于单测):按名直取、label/field selector、
      watch(含 `resourceVersion=0` 全量)、跨租户 ownerReference 触发级联删除、CRD 同名不同租户、
      PVC 绑别人的 PV、Service 指向别人的 Endpoints、SA token 换跨租户权限、`kubectl auth can-i`、
      discovery/OpenAPI 泄露
- [ ] **配额验证**(架构文档 §9,以下均为读码结论,**未做运行时复现**):
      - [ ] 生效范围:预期只有 compute 类 + `pods` 计数受"租户总量"约束;
            `configmaps`/`secrets`/`services`/`pvc`/`count/*`/`requests.storage` **无总量**,
            且 per-namespace 限额 == 总量额度 ⇒ **多建一个 namespace 就多拿一份额度**
      - [ ] ⭐ **objectSelector 绕过**:`quota.tmpl.yaml` 按 `app` 标签排除,而**标签归租户**
            ⇒ 打个标签即绕过总量约束。**修法:改为按 namespace 排除**
      - [ ] 并发超发:`UpdateQuotaStatus` 是空实现,关闭了 admission 期乐观并发记账。压测出幅度
      - [ ] 配额组件 `replicas: 1` + `failurePolicy: Fail` = 单点,改多副本 + PDB

> ⚠️ 每条测试**必须带负向对照**(确认测试真的走到了被测分支)—— 本项目在这上面栽过四次。

> lab 需求:一套 **≤ 移植后版本**的独立 kind 集群、独立端口,
> **不得影响 `kubebrain-dbaas-control-plane`**。

---

## 阶段 3:平台层

### 3.1 Kyverno(选型已定)

选 Kyverno 而非 Gatekeeper 的理由:`generate` 规则直接抵掉自研的 namespace 配套控制器;
mutation 更好写;YAML 而非 Rego。

- [ ] ⭐⭐ **铁律:所有策略匹配一律反向写**(`exclude` 平台自身 namespace,匹配其余全部)。
      **禁止用正向 selector 选租户 namespace 的标签** —— 租户能编辑自己建的 namespace,
      摘掉标签即**绕过整套策略**,此时他仍有全权建 Pod ⇒ B1 隔离前提全部落空
      - 同族问题仓库里已出现一次:配额 webhook 的 objectSelector(见 2.3)。
        **通用规则:排除条件只能建立在租户无法控制的东西上**
- [ ] 必备策略(前四条 PSA 管不了,必须策略引擎实现):
      - [ ] **P0 强制注入 `runtimeClassName=<kata>`** —— 租户不写默认是 runc(共享内核);
            且 RuntimeClass 是 cluster-scoped,租户写 `kata` 会被改写成 `<tid>-kata` 而不存在
            ⇒ **只能由平台强制注入**
      - [ ] **P0 拒绝 `spec.nodeName`**(直接绕过调度器钉到任意节点)
      - [ ] **P0 清空/白名单 `tolerations`**(否则可跑到控制面节点)
      - [ ] **P0 PSA `restricted` 等价规则** + 保护那些标签不被租户改
      - [ ] P1 限制 `nodeSelector`/`affinity` 到允许标签;强制 `schedulerName`;拒绝 DaemonSet
- [ ] 用 `generate` + `synchronize: true` 承接 namespace 配套对象(租户删了自动重建):
      per-namespace RoleBinding(1.1)、`kubetron-network` ConfigMap(3.2)、PSA 标签、
      ResourceQuota/LimitRange
- [ ] `failurePolicy` 定为 **`Fail` + 多副本 + PDB**(`Ignore` 的失效是静默的,直接击穿隔离前提)
- [ ] ⚠️ 实测租户看到的**拒绝消息**内容 —— `TrimTenantIDFromError` 能擦租户前缀,
      但擦不掉策略名和平台标签白名单(泄露面)
- [ ] ⚠️ **能不用 Kyverno 的 `context` lookup 就别用** —— 它要 cache 集群状态,
      与 4.1 是同一类全量 watch

### 3.2 kubetron 接缝(三处)

kubezoo 管控制面多租户,kubetron 管数据面/网络多租户,kata 管计算隔离。三者正交。

- [ ] **DNS zone 用租户视角的 namespace 名** —— `dns_controller.go:143` 渲染时剥掉 `<tid>-` 前缀。
      ⚠️ 这**不是 CoreDNS rewrite**(租户可建任意 namespace,rewrite 是地狱),
      而是一开始就写对名字;之所以可行,是因为 kubetron 本来就**每租户独立 zone + 独立 CoreDNS**
- [ ] **租户身份对齐** —— 建立 `kubezoo tenantID(6 位) ↔ OpenStack project / application credential`
      的一一映射,租户建 namespace 时由 Kyverno `generate` 落 `kubetron-network` ConfigMap
- [ ] **kubetron webhook 按 namespace 前缀识别租户**(前 6 位即租户 ID,与 kubezoo 模型天然对齐)

### 3.3 计算与节点池

- [ ] 平台自有 **kata 节点池**的容量规划与扩缩容
- [ ] 确认 §8 那张表列的代价可接受(DaemonSet 语义、节点级能力、冷启动、故障域)

---

## 阶段 4:规模与性能(#84 / #85)

四堵墙,按预计撞上的先后:

- [ ] **`-A` 全量 LIST 且无 cache** —— 任一租户执行 `kubectl get pods -A` 就是一次全集群 LIST;
      `pkg/proxy/` 内**零处 informer/cache**。租户越多单次越贵,**代价由全体承担**,
      同时是 DoS 面。(修法见 1.2)
- [ ] **准入 webhook 同步开销** —— 高频短任务创建 Pod,每次同步过 Kyverno,须压测
- [ ] **策略引擎的集群状态缓存** —— 与第一条同类,能不用就不用
- [ ] **上游 etcd 单一键空间(#84)** —— N 租户全部对象共用一套 keyspace,
      这是 **KubeBrain 的主场,也是产品天花板**
      - [ ] 实测键空间形态:租户前缀在 **namespace 位**,预期"每资源内连续、跨资源分散"
      - [ ] 删租户 = **每种资源一次 range 删**,不是一次连续 range
      - [ ] 与 count-index(#80)、`--keyspace`(#76,**整实例级,层次不同**)的关系
- [ ] **叠 kubegateway 做每租户限流**(#85)—— 挡"单租户拖垮全体";
      #81 已现场验证限流/熔断/降级、双网关 HA、全局限流跨副本汇总
- [ ] 规模压测形态是"**N 租户 × M 对象**",不是"1 租户 × 大量对象";
      注意 #40 的 Service 规模墙在这里可能更早撞上

---

## ⛔ 已决定不做(不要重新提议)

| 项 | 理由 |
|---|---|
| **VK + OpenStack Zun 作数据面(B2)** | VK 场景没有 kubelet ⇒ 探针/生命周期/volume 合成/SA token 续期全要重写;而"租户不买节点"两条路都成立(B1 节点也是平台的)。⇒ B2 换来的只是"把节点从 k8s 挪进 OpenStack"。调研基线保留在 #86,唯一翻案场景:容器与 Nova VM 需共用 OpenStack 同一套计算配额/调度/计费 |
| **集群内流量的透明拦截**(kubegateway 按本地端口判别租户) | 端口不能自描述,每加一租户要分配端口/改 Service/动防火墙,只换来便利性。**替代方案**:改 workload 的 `KUBERNETES_SERVICE_HOST` 指向网关主机名即可,网关零改动(Cilium 已做成 `k8sServiceHost` helm value 且 KPR 模式本就必设) |
| **OpenKruise** | CRD 租户不可见(system CRD 未实现);唯一价值 `ImagePullJob` 镜像预热的收益**取决于镜像复用率**,无数据前不据此选型。⚠️ 注意 kubezoo 的"秒级"指**租户交付**,不是 Pod 启动 |
| **CoreDNS rewrite 方案** | 租户可建任意 namespace,要覆盖 `<ns>.svc → <tid>-<ns>.svc` 全部形态还要管反向解析/SRV/headless,是地狱。改用 kubetron 的每租户独立 zone(见 3.2) |
| **每租户一个 LoadBalancer 地址** | kube-proxy DNAT 会把目的 IP 换成 pod IP,N 个 Service 塌缩成同一个 `LocalAddr`,只有端口能幸存 |

---

## 诚实边界(架构文档 §14 的摘要)

架构判断建立在**源码阅读 + 各项目自身的测试报告**上,不是端到端实测:

- **kubezoo + kubetron 这个组合从未运行过**;3.2 的三处接缝均为纸上推演
- kubetron 的数据(e2e 76/76、SVC 13/13、kata-fc 5.4s Ready、Knative 冷启动 5.3s)
  是其自测结果,非本项目测量,亦非生产验证
- 阶段 2 配额那三条、Kyverno 拒绝消息泄露面,均未做运行时复现

准确表述:**未发现结构性障碍,且工程量比 B2 小一个量级** —— 而非"已验证可行"。
