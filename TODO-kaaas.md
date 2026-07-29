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

### 0.3 编译修复 ✅

- [x] `pkg/` 全部 + `cmd/clusterresourcequota` 编译通过 — `58959f1` `22e6133` `6ce59fc`
- [x] `cmd/kubezoo/app/customresource_handler.go` — `ec1827f`(见 0.4)
- [x] `cmd/kubezoo/app/server.go` — `60e130b`。实际改动比预估的三条多:
      `Features.ApplyTo` 参数 / `genericConfig.Version` 移除 / `storageFactoryConfig.Complete` 返回值,
      外加 `ToAuthorizationConfig` 加 error、authenticator `New` 收 ctx 返 5 值、
      `HandlerChainWaitGroup` 拆分、`WithAuthentication` 加 RequestHeaderConfig、
      `IsValidServiceAccountKeyFile` 移除
- [x] `go build ./...` 全绿 — `60e130b`

### 0.4 CRD handler 重新 fork(#88)✅ — `ec1827f`

按三方合并做完(base=上游 v1.24.0 / ours=kubezoo fork / theirs=上游 v1.36.3),
14 个冲突块全解,约 900 行上游演进自动合入,文件 1450 → 1624 行。

- [x] 解 14 个冲突块
- [x] kubezoo 语义全部保住:`getTenantCRD` / `util.ConvertCRDNameToUpstream` /
      每个 verb 的 `proxyStorage` / nil admission 链 / `upstreamConfig`
- [x] 确认已解的 1.36 改动没被回退(managedfields、`map[string]*spec.Schema`、
      openapi V3、`NewExcludeFilterSetMap`、ServerSideApply 门移除)
- [x] 对齐剩余 1.36 API(`GetRESTOptions` 加参数等)

⭐ **解冲突时发现的两件事,值得记住**:

1. **kubezoo 的 fork 基线其实比 1.24 更老** —— 它缺 `StrictSerializer`、缺
   `StorageObjectCountTracker`、`unstructuredSchemaCoercer.apply` 还是单返回值。
   所以多数"冲突"是基线错位造成的**假冲突**,upstream 侧直接就是对的。
   顺带补回了整条 **unknown-field-paths / strict-decoding 管道**(此前完全没有)。
2. **一处有意与上游分道**:上游 1.26 把"CRD terminating 时拒绝 create"从直接报错
   改成用 `forbidCreateAdmission` 包住 admission 链。那个 wrapper 会解引用 delegate,
   而 kubezoo 传的是 **nil admission 链**(admission 在上游 apiserver 跑),所以这里
   保留直接拒绝。文件里有注释说明。
3. `CRDRESTOptionsGetter` 本地副本已删 —— 上游挪了位置,`apiextensions.go` 早就在用
   `apiextensionsoptions.NewCRDRESTOptionsGetter`。

### 0.5 收尾

- [x] ⚠️⚠️ **`go test ./...`** —— 已跑,`make test` 全绿 — `60e130b`
      - 单元测试全过;`pkg/controller` 集成测试在**真 etcd + kube-apiserver 1.36**
        下通过(envtest 从 1.24 抬到 1.36,`ENVTEST_K8S_VERSION` / `SETUP_ENVTEST_VERSION`)
      - `NewQuotaConfigurationForAdmission(nil, nil)` 传 nil:测试通过,但**没有构造
        DRA 场景**去打那条唯一会解引用 informer factory 的分支
      - 修掉的:client-go 聚合发现改写导致 `/apis` 响应丢 `kind`/`apiVersion`
        (**真回归**,已确认兄弟端点 `/apis/{group}` 与 `/apis/{group}/{version}` 不受影响);
        两处测试夹具的类型更名(gnostic→gnostic-models、PVC 的
        `ResourceRequirements`→`VolumeResourceRequirements`)
- [x] 二进制能启动 — `fe885dd`。⚠️ **"编译过 + 测试绿"没能挡住启动即 panic**:
      `AddCustomGlobalFlags` 去全局 flagset 找 `default-not-ready-toleration-seconds`,
      1.36 已把它挪进 `AdmissionOptions.AddFlags`,`globalflag.Register` 找不到就 panic。
      整个函数已过时,连同 `globalflags.go` 一起删掉
- [x] `apigroups.go` 按 1.36 **全面核对**资源/版本清单 — `3747fb7`。1183 → 935 行。
      方法:envtest 起**真 1.36 apiserver** 导出 discovery 当基准,与表格机器比对(不靠记忆)
      - **删掉 12 个陈旧 group-version**。其中 7 个(`extensions/v1beta1`、`policy/v1beta1`、
        `batch/v1beta1`、`discovery/v1beta1`、`authorization/v1beta1`、`rbac/v1alpha1`、
        `rbac/v1beta1`)1.36 apiserver **完全不认识**;另 5 个"还认识但默认关闭",且**已被改作
        完全不同的资源** —— 显式 `--runtime-config` 打开后实测:
        `admissionregistration/v1beta1` 给的是 mutatingadmissionpolicies(不是 webhookconfigurations)、
        `coordination/v1beta1` 是 leasecandidates(不是 leases)、
        `networking/v1beta1` 是 ipaddresses/servicecidrs(不是 ingresses)、
        `authentication/v1beta1` 与 `certificates/v1beta1` **什么都不给**
      - ⭐ **这些不是死代码,是活端点**:`NewRESTStorage` **忽略了传进来的
        `APIResourceConfigSource`**,表里写什么就装什么,而 scheme 仍给这些版本优先级 ⇒
        kubezoo 真的对外提供 `/apis/extensions/v1beta1/...`,转发到上游得 404。
        副作用是 **`--runtime-config` 对所有被代理的 group 全部失效**。已改成按 config source 过滤
      - 补两个 1.36 有而这边缺的:`pods/resize`、`persistentvolumeclaims/status`
        (后者是纯遗漏 —— 其他 core 资源都有 status 子资源)
      - **加了两个常驻守卫测试**(都在旧表上验证过会红,第一个抓 7 个、第二个抓全部 12 个):
        声明的 GV 必须是 1.36 apiserver 认识的;声明的每个资源必须能活着穿过 resource-config 过滤
      - ⚠️ **有意没加,需要产品决策而非移植决定**:上游启用但这边没有的
        `storage.k8s.io/v1`(StorageClass!PVC 要引用它)、`scheduling.k8s.io/v1`、
        `certificates.k8s.io/v1`、`resource.k8s.io/v1`(DRA);以及已暴露 group 内部的
        `admissionregistration/v1` 策略对象、`networking/v1` 的 ipaddresses/servicecidrs
        —— 后两类是**集群级配置,租户大概率不该碰**
- [x] `pkg/util/util.go` 的 `groupKindNamespaced` 表按 1.36 更新 — `ad3e45c`。54 → 68 条
      - 这张表决定 ownerReference/objectReference 走哪半边改写:**集群级 ⇒ 给 name 加租户前缀,
        namespaced ⇒ 不加**(前缀在 namespace 上)。把集群级误标成 namespaced,
        两个租户在同名集群对象上就直接撞车
      - ⭐ **54 条已有条目作用域全部正确,零错** —— 这是个干净的负面结论,不是修复
      - 真正的问题是**覆盖不全**:16 个 apiserver 提供的 kind 不在表里 ⇒ `IsGroupKindNamespaced`
        报错 ⇒ 落到 `unregistered crd group`。其中 `resource.k8s.io/ResourceClaim`、
        `ResourceClaimTemplate` 是**租户 Pod 现在就可能引用**的。已全部补上
      - 删 `extensions/Ingress`、`policy/PodSecurityPolicy`(kind 已不存在)
      - ⚠️ `autoscaling/Scale` 看着也像退役,**实际没有** —— 它作为 `deployments/scale` 的 kind
        还活着(namespaced=true)。核实过才没误删
      - **守卫测试**对比表与真实 discovery(旧表上精确报出 16 缺 + 2 陈旧)。两个必须做对的点:
        只有**顶层资源**算基准(只有它们能被引用;子资源的 kind 是请求体,如 `PodExecOptions`/
        `Eviction`/`TokenRequest`,不该进表);**只作为子资源 kind 出现的不算陈旧**(即 Scale,
        discovery 把它挂在父资源的 group 下,不是表里用的 autoscaling)
      - 需要真 apiserver ⇒ 归入 `make test-integration`(现在也覆盖 `pkg/util`),单元跑时 skip
- [x] `make codegen` 重新生成 + `make verify-codegen` 通过 — `d7bb1fc`
      - ⚠️ **改之前 `make verify-codegen` 是通过的,而且毫无意义** —— 配方(`4fadaa5`,我自己写的)
        有两个洞:① `install_gen` 只按**二进制名**判断是否已装,不看版本 ⇒ 依赖抬升后一直在用
        **1.24 时代的生成器**跑 1.36 类型;② 两个 openapi 目标结尾是
        `| grep -v 'API rule violation' || true`,`|| true` 把**所有失败**都吞了。
        旧生成器实际是**硬失败**的(`AzureDiskVolumeSource.CachingMode` 的 `+default=ref(...)`
        它解析不了)⇒ 什么都没生成 ⇒ diff 无从比较 ⇒ **verify 对它唯一要守护的那个文件报了成功**。
        换成 go.mod 钉的生成器后该错误消失(新版认识 `ref()`),纯属旧二进制的产物
      - 换正确生成器后命令行要迁到 **gengo v2**:输入变位置参数、
        `--output-base/--output-package/--output-file-base` → `--output-dir/--output-pkg/--output-file`;
        **deepcopy/defaulter/register 已经没有输出目录了,直接写在输入包旁边** ⇒ staging 模型失效,
        `--verify` 改为把模块复制到临时树、在那里生成、再整树 diff
      - 全量重新生成:`pkg/apis/openapi/zz_generated.openapi.go` 54776 → 65753 行(1.24→1.36 类型集)
      - **双向验证过**:刚生成完的树上 verify 通过;篡改生成文件里的一行 ⇒ verify 精确报错
      - ⚠️ **教训同"编译过≠能跑"**:`make verify-codegen` 通过也不等于配方是对的。
        新增/修改校验类脚本时,**必须做一次负向对照**(故意弄脏,确认它会红)
- [ ] **带证书 + 真实存储(KubeBrain)把 kubezoo 跑起来,发一个租户对象**
      —— 目前最强证据止于"能启动、参数校验正常报错",**没有服务过一个真实请求**
- [x] 合并回 main — `ec374da`(`--no-ff`,本地已合,**未推送**)
      - 三个 `WIP:` commit 压成一个诚实的 `Move the build onto Kubernetes 1.36.3`
        (它单独仍编不过,提交信息里写明了)
      - ⚠️ **合并前抓到:`58959f1` 误提交了 75MB 构建产物 `clusterresourcequota`**,
        且一路留到了分支顶端。已剔除,并把 `/kubezoo`、`/clusterresourcequota` 加进 `.gitignore`
        (`hack/build.sh` 会在仓库根目录落这两个文件)
      - 重整历史的保障:**新旧分支最终树逐字节比对**,除 `.gitignore` 与被删二进制外零差异
      - 合并后在 main 上重跑:build / `make test` / `make verify-codegen` / 二进制启动,全绿

> ⚠️ **方法学**,两条都在这次移植里应验了:
> 1. **"还剩 N 个错误"不是可靠进度指标** —— 每修好一处就暴露下一处(server.go 曾
>    6→3→又 6,最后实际改了 8 类 API 而不是预估的 3 类)。排期别按错误数估。
> 2. **"能编译"和"测试绿"都不等于"能跑"** —— 全树编译通过、`make test` 全绿之后,
>    二进制仍然启动即 panic。**每个移植里程碑都要真正执行一次产物**,而不是只看构建结果。

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
