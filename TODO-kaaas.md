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
- [x] **带证书把 kubezoo 跑起来,发一个租户对象** — `32be3a0`(#89)
      - ⭐ **存储用 etcd 不用 KubeBrain**(用户定):本项验的是"1.36 上能否正确服务请求",
        与存储后端语义无关;上 KubeBrain 还要拖 PD/TiKV。kubezoo × KubeBrain 归 #84
      - 上游用 kind v1.35(1.36 尚无 kind 节点镜像),隔离 kubeconfig,未碰 `kubebrain-dbaas`
      - ⭐⭐ **"编译过 + 测试绿 + verify-codegen 绿 + 二进制能启动"之后,仍然一个请求都服务不了。
        三个编译器完全看不见的缺陷:**
        ① `OpenAPIV3Config` 没设 —— fork 时可选,SSA 转正后 `getOpenAPIModels` 靠它建字段管理
           类型转换器,为 nil 直接拒绝启动
        ② 喂给它的定义只有 `pkg/apis/openapi`(被代理的 k8s 类型),**自有类型 Tenant 在
           `pkg/apis/generated/openapi`** —— 以前只喂 /openapi 端点无所谓,现在要为**每个已装
           资源**建转换器,缺一个就致命。已合并两份
        ③ tenant store 缺 1.26 起必填的 `SingularQualifiedResource`。本该是一句清晰报错,
           但 `v1alpha1Storage` **把 `NewREST` 的 error 丢了**(`tenantStorage, _ :=`),
           于是 typed-nil 进了 storage map,**首次方法调用直接 segfault**
      - 另:代理 storage 缺 1.26 起必需的 `GetSingularName`。规则用"Kind 小写"——
        对着真 1.36 apiserver 核过,68 个带单数名的资源**全部**等于 `ToLower(Kind)`,零例外
      - ⚠️ **两个随仓库发布的文件都传了已被 klog 移除的 `--logtostderr`**:
        `hack/lib/gen_pki.sh`(文档让你照抄它打印的参数)和 `config/setup/all_in_one.yaml`
        (**部署到集群里同样起不来**)。是脚本 30 个、清单 33 个参数里唯一失效的
      - **端到端验收结果**:租户看到 `default/test` 而上游是 `111111-default/test`,
        且看不到平台自身 pod;两租户同名 ConfigMap 各读各的、互相看不到对方 pod/CRD;
        租户 CRD 是 `foos.stable.example.com` 而上游 `foos.111111-stable.example.com`,
        CR 实例经**重新 fork 的 CRD handler** 得到 `stable.example.com/v1 default/myfoo`
        vs 上游 `111111-stable.example.com/v1 111111-default/myfoo`;
        `/apis` 重新带上 `kind`/`apiVersion`(本轮修的回归现场坐实)
      - ⚠️ 差点误报一次:`get pod -A | wc -l` 把 "No resources found" 数成了 1,
        看着像跨租户泄露。**看原始输出才确认零串扰** —— 计数类断言必须回看原文
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

### 1.1 per-namespace RBAC(#87)✅ — `e01e169`

原状:每租户在上游被授予 `*` on `*`,kubezoo 自身 `--authorization-mode=AlwaysAllow`
⇒ **隔离完全依赖改写层,零兜底**。之所以值得做:租户身份**确实到得了上游** ——
`pkg/dynamic` 每个请求都打 impersonation 头,上游按 `<tid>-admin` 判权,只是被设成了全放行。

- [x] namespaced 半边改为**每个租户 namespace 一个 RoleBinding**(引用共享 ClusterRole,
      由 binding 所在 namespace 完成约束;role 保持宽泛,以覆盖租户 CRD 的自定义资源)
- [x] cluster-scoped 半边**逐条枚举**(= apigroups.go 提供的集群级资源 + CRD),
      非资源 URL 不再授权(discovery 靠 k8s 自带的 `system:discovery` 绑定)
- [x] 验收:上游 RBAC 真拒绝,**每条带负向对照** —— 给临时用户绑上旧的 `*` on `*`,
      同样六个问题全部答 yes,证明那些 no 是 RBAC 拒绝而非别的原因失败
      · 自己 ns 建 pod ✓ / 自建 ns 建 cm ✓ / 跨租户建 pod ✗ / 跨租户读 secret ✗ /
      读 kube-system secret ✗ / 全集群 list pods ✗
- [x] **限制租户对自己 namespace 的 update(防摘除策略标签)—— 实测已经是安全的,不需要改**。
      `NamespaceTransformer.Forward` 无条件重写 `kubezoo.io/tenant`,而 patch 走
      `guaranteedUpdate → update` 也过转换器,所以四种摘标签写法(label `-` / merge null /
      json remove / 改成别人的 id)上游标签一字未变,改成别人的直接被拒。
      这条是 A 的 webhook 收口与退租强制清理**共同的地基**,所以专门验过。
      ⚠️ 前两种写法 kubectl 回显像是成功了,实际没变

⚠️ **四个差点让功能"看着做完实则没做"的点**:

1. **reconciliation 默认只增不减** ⇒ 光收窄规则,已有集群上的 `*` on `*` 原封不动,
   降权只对新租户生效却显得已全量生效。必须显式 `RemoveExtraPermissions`。
   同理 ClusterRole **保留了名不副实的旧名字** —— 改名会把旧 role/binding 留在集群里继续授予 cluster-admin
2. **租户 namespace 不是固定集合** —— 已预判但第一版仍踩:**现场观察到**租户建完 namespace
   后在自己的新 ns 里被拒。改为按 `kubezoo.io/tenant` 标签 watch namespace,约 **2 秒**可用
3. **那个 watch 一开始是哑的** —— `onTenantUpdate` 根本不调 `syncResources`。
   顺带暴露既有缺陷:**resync 产生 Update,所以租户的上游 ns/RBAC 自创建后就再没被收敛过**,
   删掉一个要等 kubezoo 重启才回来。已修;但**删除期间必须跳过**(否则把 finalizer 刚拆的建回去)
   —— 这条是**仓库自带控制器测试**抓到的
4. ⚠️ **我误判并删错过一次**:`<tid>-admin` 聚合 ClusterRole 无任何 binding 引用,判为死代码删掉;
   测试红后查明 **租户 RoleBinding 引用内置 `admin` 会被改写成 `<tid>-admin`**,必须存在。已恢复。
   顺带发现 `edit`/`view` 无对应镜像 ⇒ 租户引用会悬空(**该缺陷早于本次改动**)

**守卫测试**:集群级授权清单与 `apigroups.go` 漂移即报错,两个方向都管
(少授权 ⇒ 租户运行时 Forbidden;多授权 ⇒ `*` on `*` 借重构复活)。

> ⚠️ **硬边界**:RBAC 的 `resourceNames` 是**精确匹配**,无通配无前缀
> (`k8s/pkg/apis/rbac/v1/evaluation_helpers.go:86`)。
> Namespaced 资源(40+ 种)✅ 可兜底;**cluster-scoped(PV/StorageClass/Namespace/CRD… 20+ 种)
> ❌ 永远只能靠改写层**。
>
> **本项已完成,#82 的性质因此从"必须零遗漏否则即越权"降为"找漏,漏了还有网"** ——
> 但**只对 namespaced 资源成立**,约 15 个集群级资源仍是零兜底。

### 1.2 已知缺陷修复

- [x] **Node 对所有租户无条件可见** —— 已修并实测。
      ⭐ **这条 TODO 只点了一处,实际有三处**:删掉 `pkg/util/util.go` 那个分支之后
      `get nodes` 空了,但 **`get node <名字>` 照样返回完整对象** ——
      `pkg/proxy/proxy.go` 的 Get 路径里还有一个独立豁免让 Node 跳过名字前缀转换;
      第三处是 `pkg/convert/init.go` 把 Node 映射到 `nopeConvertor`。
      **与 B(PV)同一个形状**:nope 转换器 + 读路径按前缀过滤。
      三处各带一条 TODO 注释,**按注释文案 grep 只能找到一处**
      - 复测:list 空 / get NotFound / raw GET 404 / watch 静默 / 平台自己不受影响
      - 顺带删掉另外两个 `nopeConvertor` 条目(`PriorityClass` 不服务、`PodSecurityPolicy` 1.25 已删)。
        现在**没有"什么都不做"的转换器条目了** —— 若按 I 的 A 方案把 PriorityClass 服务出去,
        那条 nope 会原样复现 PV 的 bug
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

> ⭐ **审计报告见 [`docs/isolation-audit-cn.md`](docs/isolation-audit-cn.md)**(真实上游集群双租户实测,
> 每条注明实测/读码)。以下勾选项按该报告。

- [x] **静态覆盖审计** —— 枚举转换器接线情况,发现两个**写好但从未接线**的转换器
- [x] **双租户黑盒穿越测试**(真 kubectl,kind v1.35 + 移植版 kubezoo + etcd)

### 审计结论:三条待修 + 两条已知

- [x] ⛔ **A 最高(已修 `5609db5`):租户可注册全集群生效的准入 webhook** —— `apigroups.go` 暴露了
      mutating/validating webhookconfigurations,而 `pkg/convert` **完全没碰它们**:
      `rules` 不带租户范围、`namespaceSelector` 空 ⇒ 匹配全集群;`clientConfig.service`
      不加前缀 ⇒ 指向平台命名空间。**实测:一个租户的一个对象同时打死另一个租户和平台自身**。
      改 `failurePolicy: Ignore` + 指向自己可达的服务,即可**读到全集群每个被创建的对象**
      ⚠️ **per-namespace RBAC(#87)挡不住** —— 集群级资源,`resourceNames` 表达不了名字前缀
      · **已定案并实现:转换层强制改写**(用户定"按生产准入修,不能留纰漏")。四处强制缺一即有出口:
      `clientConfig.service.namespace` 加前缀 / `namespaceSelector` 强制为租户标签 /
      每条 rule 的 `scope` 强制 `Namespaced`(⚠️ 最易漏 —— nsSelector 对集群级资源不生效) /
      `clientConfig.url` 直接拒绝。**CRD 的 `spec.conversion.webhook.clientConfig` 同样收口**
      (它是第二条同类路径,"不暴露 webhookconfigurations"堵不住它)
- [x] ⛔ **B 高(已修 `5c90901`):PersistentVolume 走 `nopeConvertor`,完全未改写**。写路径不加前缀、
      读路径按前缀过滤,两边不对称 ⇒ ①上游是裸名 ②**创建者自己都 get 不到** ③另一租户建同名得
      `AlreadyExists`(**跨租户存在性泄露**)④谁都删不掉 ⇒ **对象永久滞留、名字被永久占用**
- [x] ⚠️ **C 中(已修 `5c90901`):PVC 的 `spec.volumeName` 未改写**。
      配合 B,**PV↔PVC 绑定整条链路没有转换**
      ⚠️ **迁移**:修复前产生的裸名 PV 仍滞留上游且对所有人不可见,需运维手工清理(非本次引入)
- [x] **D 成因已定位**:`NewPVTransformer` / `NewPVCTransformer` 实现完整、**单元测试都通过**,
      但 `init.go` **从未注册它们**(各被引用 0 次)。
      ⭐ **方法学:单元测试测的是转换器本身,没有任何测试检查它是否被接上**
      · **已加两个"接线守卫"测试**(webhook 与 PV/PVC 各一),均验证过摘掉注册会报红
- [x] **E 已知项现场确认**:Node 对所有租户可见(见 1.2)

### 通过项(实测正向对照)

namespace/name 前缀 · CRD 同名隔离(`widgets.111111-...` vs `widgets.222222-...`) ·
Service/Endpoints 转换 · 跨租户 ownerReference(悬空后被 GC 收走,k8s 本身禁止跨 ns owner) ·
上游 RBAC 兜底(#87,带负向对照)

### 审计期间顺带发现并修掉的两条(与隔离无关但更致命)

- [x] ⛔ **所有 PATCH 请求 panic** — `fce8bf9`。kubezoo 自建的 handler chain 漏了
      `WithAuditInit`(上游 `DefaultBuildHandlerChain` 总会装,因为审计辅助函数**无条件解引用**
      AuditContext)⇒ `audit.LogRequestPatch` 空指针。影响 `kubectl annotate/label/patch/scale/set image`
      ⚠️ **它还推翻过一个结论**:我验证"租户不能 update 改宽 webhook"时用的正是 `kubectl patch`
      且吞了 stderr —— 那次请求其实 panic 了,**验证什么都没证明**。修后用 PATCH + PUT 重做,结论成立
- [x] ⛔ **退租时租户可留下 finalizer 永久卡死自己的 namespace** — `fce8bf9`。一行 YAML 即可。
      爆炸半径实测:其他租户与平台不受影响,但**该租户 ID 被毒化** —— 同 ID 重开会得到半残租户
      (4 个系统 ns 回来 3 个,租户第一个请求就 Forbidden)。
      修法(用户定 A):退租**最后**强制清理全部 namespaced 资源的 finalizer ——
      此时 ns 已 Terminating(不收新对象)、集群级绑定已删,不存在被重新挂上的窗口。
      实测 4 个地雷(3 种类型 2 个 ns)10 秒内清干净
- [x] 修掉本会话 #87 引入的重试循环:namespace watch 对 terminating 的 ns 也入队 ⇒
      租户已删时产生持续 `tenants not found`。现跳过 terminating ns,租户 NotFound 视为无事可做

### 尚未覆盖(不算做完)

- [x] **配额三条已真部署跑完** — `docs/isolation-audit-cn.md` H 节
      - ⭐ **部署即撞到移植期的错误假设**:`NewQuotaConfigurationForAdmission(nil, nil)` ——
        我曾记"只有 DRA 两个 gate 同时开才解引用,没测过",**该假设是错的**:
        `DynamicResourceAllocation` 1.35 起 GA+LockToDefault、`DRAExtendedResource` **1.36 转 Beta 默认开**
        ⇒ 组件**一启动就 CrashLoop**。修法:明确告诉配额配置 kubezoo 不服务 `resource.k8s.io`,而不是塞一个用不上的全集群 Pod informer
      - [x] ⛔ **生效范围坐实**:每个租户 namespace 各拿**一份完整额度**。声明 cpu=4,
        4 个系统 ns 即 16 core,租户自建 2 个后 **24 core(6 倍)**,随 namespace 数无限增长
      - [x] ⛔ **objectSelector 绕过坐实并已修**:超额 Pod 打上 `app: kubezoo-cluster-resource-quota`
        即**创建成功并落地上游**(经租户正常路径)。**修法:排除条件改为按 namespace** ——
        namespace 不归租户控制。修后同一请求被拒,平台组件仍能自愈重建
        ⇒ 正是 3.1 铁律的实例:**排除条件只能建立在租户无法控制的东西上**
      - [ ] ⚠️ **并发超发:代码确认、行为未复现**。`UpdateQuotaStatus` 确是空实现
        (`webhook.go:190` 直接 return nil),但并发 6×2core 打 4core 配额只落地 2 个。
        **不写成已坐实**,需更高并发/更小时窗
      - [x] **单点坐实**:`replicas: 1` + `failurePolicy: Fail` + 无 PDB
      - ⚠️ **方法学(踩了三次)**:配额测试对环境状态极敏感,连续三次拿到无效对照
        (脚本参数错位没加上标签 / 前一步删过 Pod 导致用量归零 / `nodeName` 造假让 Pod 被 GC)。
        **每次都"看起来得出了结论"。先确认起点状态,再做单一变量对照。**
- [x] watch / field selector / 发现面 —— **实测通过**(见 `docs/isolation-audit-cn.md` 通过项)
      - watch 从 `resourceVersion=0` 看集群级资源:只回自己的,且做了去前缀转换;
        负向对照是"窗口内平台和另一租户都写了东西"
      - namespace 名字花招无效;⚠️ 差点误判:错误文案被 `TrimTenantIDFromError` 擦过前缀,
        **看着像没加前缀**。错误文案不能当证据,要看上游落地的对象名
- [x] ⛔ **三个"类"引用字段坐实,比读码结论更重** —— I 节。不只是悬空,还能引用平台的:
      - `runtimeClassName`:自己的建不出来(Forbidden),**平台的 `kata` 建得出来**
        ⇒ B1 架构里租户写 `runc` 就跑在 kata 沙箱外,**承重**
      - `ingressClassName`:自己的**静默失效**(Ingress 无准入期存在性校验),平台的直接接上控制器
      - `priorityClassName`:`system-cluster-critical` ⇒ **priority=2000000000**,
        全集群最高,抢占其它租户(已核 1.36 源码:该类不再有 kube-system 限制)
      - [ ] 定架构再改:**不能简单加前缀**(会让租户永远用不了平台共享类)。
        A=引用加前缀+按租户投影一份平台共享类(与"system CRD 共享机制"同一个坑,合并设计);
        B=不改写但用 Kyverno 白名单限死取值(`priorityClassName` 只能走 B)
- [x] ⛔ **`kubectl auth can-i` 对租户全错** —— J 节。SAR 的 `resourceAttributes.namespace` 不转换。
      判据是四行对照:租户问=no / 租户做=成功 / 上游问转换后的 ns=yes / 上游问未转换的=no。
      ⚠️ **#87 之前看不见**(那时 `*` on `*` 问什么都回 yes)—— 这类缺陷会跟着每次权限收紧冒出来
      - [x] **已修并复测**:`pkg/convert/accessreview.go`,四个 kind 全接线
        (含 `SelfSubjectRulesReview`),转 namespace + 自定义资源组,主体搬进租户身份空间,
        **裸 `system:` 主体直接拒绝**(那是平台身份,拿它提问=读平台 RBAC)。
        `can-i create pods -n default` 现在与实际动作一致
      - [ ] ⚠️ 残留(**已证明是 vanilla 行为**):集群级资源上 kubectl 仍带当前 namespace,
        租户在自己 ns 里是 `*` on `*` ⇒ 回 yes 而真实请求 Forbidden。
        **对照**:上游给普通 user 绑同样的 namespaced `*` on `*`,行为一模一样。
        要比 vanilla 更准需按资源判作用域后清空 namespace(`resourceAttributes` 只有复数 resource 没 kind)
- [x] ⛔ **两个子资源解不出请求体** —— K 节。`serviceaccounts/token`(`kubectl create token` 唯一取法)
      与 `pods/eviction`(PDB 生效路径)沿用了父资源 Kind。`scale` 没事是因为**只给 scale 想到了**
      - [x] **已修并复测**。⭐ 同源的**第三个**是 `pods/binding`;
        且改完 body kind 后 `create token` **仍失败**(`name is required`)——
        父对象名字 `pkg/dynamic` 是从 **body 的 metadata.name** 取的,
        eviction/binding 按惯例带所以看不出,**TokenRequest 不带**。名字本就在**路径**里。
        已加 `CreateSubresource(ctx, name, ...)` 显式传名。
        复测:token 出 JWT / eviction 返回 Eviction / binding 变成正常的 Conflict
- [x] ⛔ **`/openapi/v2` 原样透传** —— L 节。任一租户能枚举其它租户 id + CRD 组名/Kind/schema;
      自己的 CRD 也在错误组名下 ⇒ `kubectl explain widget` 失败而 `get widgets` 正常。
      对照:自己的也是上游名字 ⇒ **整条路径一次转换都没有**。`/openapi/v3` 与 `/apis` 干净
      - [x] **已修并复测**:先**按归属删**(只有顶层键能判归属),再**整篇剥本租户前缀**
        (删干净后每次出现都是前缀,无论在键 / `$ref` / `x-kubernetes-group-version-kind` 里)。
        两租户互为对照 × JSON/protobuf 两种编码:残留前缀 0、对方条目 0、悬空 ref 0、原生面完好
      - ⚠️ **中途踩到一次**:先写的版本只改键不改体,protobuf 那路又"只剥自己不删别人"——
        gnostic 把 paths 表示成**具名数组而非 JSON 对象**,基于 map 的删除**静默无事可做**,
        文本剥离却照跑。**"函数跑了"≠"函数做了事"**,判据只能是输出
- [x] ⚠️ **新发现 M:`/openapi/v3` 里根本没有租户的 CRD** —— kubezoo 服务的是自己聚合的静态文档,
      两租户逐字节相同。**隔离上没问题**(不含任何租户内容),但 `kubectl explain` 默认走 v3
      ⇒ 对租户自己刚建、`kubectl get` 完全正常的 CR 报找不到资源;
      `--output=plaintext-openapiv2` 才能用(这也正是 L 修好的证据)
      - [x] **已修并实测**:两半**分别取**——原生那半继续用 kubezoo 自己的
        (上游那份描述的是上游 apiserver,含 kubezoo 不服务的资源如 `resource.k8s.io`,
        照抄等于广告给租户);自定义那半只能来自上游,按归属过滤+剥前缀。
        索引用 `responseRecorder` 接住下游输出再合并;上游取不到时**降级为只回原生面**
      - 验收:`kubectl explain widget` / `widget.spec.size` 都正常,
        租户 222222 explain widget = "doesn't have a resource type",原生 explain 不受影响;
        取对方的 GV 文档 404;服务给租户的 schema 与上游逐字段相同(含 description)
      - ⚠️ **差点误判**:改完第一次测仍报一样的错 —— **kubectl 把 openapi 缓存在 `~/.kube/cache`**。
        **客户端缓存会让服务端的修复看起来没生效**,清缓存后才是真结果

> ⚠️ 每条测试**必须带负向对照**(确认测试真的走到了被测分支)—— 本项目在这上面栽过四次。

> lab 需求:一套 **≤ 移植后版本**的独立 kind 集群、独立端口,
> **不得影响 `kubebrain-dbaas-control-plane`**。

---

### 1.4 ⛔ codegen 漏了 protobuf —— 已修(#90 的前置)

做 #90 要给 Tenant API 加一个字段,加完发现**字段静默不落盘**:

```
kubectl apply  → tenant.tenant.kubezoo.io/444444 created     # 成功
kubectl get    → {"id":444444,"quota":{...}}                 # suspension 不见了
```

**同一个对象里就有铁证**:`last-applied-configuration` 注解记着客户端**发过**
`suspension`,而存下来的 `spec` 没有。丢在服务端,不是客户端也不是存储层。

⭐ 根因:kubezoo 自己的对象**以 protobuf 为存储媒介**
(`options.go` 明写 `DefaultStorageMediaType = application/vnd.kubernetes.protobuf`),
而 `hack/make-rules/codegen.sh` 的目标是 `deepcopy defaulter register openapi openapi-served client`
—— **没有 protobuf**。`generated.pb.go` 是签入的,**仓库没有任何办法重新生成它**。
于是 deepcopy / openapi / client 全都刷新了,树看着是新生成的,而**唯一决定什么能落盘的那个文件是陈旧的**。

⚠️ **我中间判错过一次,记下来**:先用 `--storage-media-type=application/json` 做 A/B,
字段**仍然丢**,我据此排除了 protobuf。后来把类型单独拿出来跑
`Marshal`→`Unmarshal` 往返,**字段确实被丢掉且不报错** —— protobuf 就是原因之一。
那次 JSON A/B 为什么也丢,我**没有查清楚**,不写成结论。
**孤立复现 > 部署态 A/B**:部署态变量太多,一次没生效就会得出反向结论。

修法:给 codegen.sh 加 `protobuf` 目标(`go-to-protobuf`)。四个坑依次踩过:
GOPATH 形状的输出树(用软链接把模块路径指回工作树)、
apimachinery/api/gogo 三个 `.proto` 的 import 路径、`goimports` 没装、
以及 **`k8s.io/api/core/v1` 必须标成 import-only**,否则 quota 的 `.proto`
会把 core/v1 的消息**本地重新声明一遍**(编译直接报 undefined)。

⚠️ 重新生成后 quota 的 `generated.pb.go` **少了 172 行**
(`ProtoMessage()` / `Descriptor()` / `XXX_*` 全没了)。**不是回归** ——
核对了上游 k8s 1.36 自己的 `k8s.io/api/core/v1/generated.pb.go`,同样一个都没有;
签入的那份是 1.24 时代生成器的产物。`.proto` 一字未变 ⇒ 线格式不变。

- [x] `codegen.sh` 加 protobuf 目标;`make verify-codegen` 现在也覆盖它
- [x] 加**永久守卫** `pkg/apis/tenant/v1alpha1/protobuf_test.go`:
      把填满的对象过一遍自己的 marshaller,字段掉了就报红(已验证:换回旧 pb.go 立刻红)
- [x] Tenant API 加 `spec.suspension`(`mode: ReadOnly|Revoked` + `reason`),**已能正确落盘**
- [x] **执行逻辑已实现并实测**(两层),详见架构文档 §9.5:
      前门 `pkg/filters/suspension.go` 按模式拒绝并给出可读文案;
      控制器按模式收窄/撤销上游 RBAC(ReadOnly 换只读 roleRef —— `roleRef` 不可变,
      靠 reconciliation **recreate**;Revoked 删绑定)
      - 实测四段:正常 → ReadOnly → Revoked → 解除,**Pod 全程 Running / restarts=0**,
        解除后 roleRef 自动恢复成 admin、租户可写
      - 两种模式都保留 discovery,否则 kubectl 在构造请求阶段就挂,租户看到的是"客户端坏了"
      - ReadOnly 放行 `authorization.k8s.io` 的 review,拒绝 `exec`/`attach`/`portforward` 与 `serviceaccounts/token`
- [ ] ⛔ **Revoked 不覆盖租户自建的 RoleBinding —— 实测坐实,未实现中和**:
      租户把 `admin` 绑给自己的 SA,吊销后该 SA 仍 `can-i delete pods = yes`,
      **能删证据**。欠费场景这是对的,取证场景是洞。
      中和需要连带"可还原",没做;在做出来之前控制器**每次 Revoked 都打警告并列出具体绑定**。
      ⚠️ **现阶段 Revoked 不能单独当取证冻结用**

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
