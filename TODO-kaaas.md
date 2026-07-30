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

## 阶段 0:移植到 k8s 1.36.3(#83 / #88)✅ 全完

`k8s.io/*` 全族 **1.36.3**(staging `v0.36.3`),Go 基线 1.26.0,生成代码重新生成并有
`make verify-codegen` 守卫,CRD handler 已按 1.36 重新 fork。逐条清单见 git log,
经验记录见 `docs/modernization.md`。

⭐ **只留最贵的三条教训**:

1. **编译过 + `make test` 绿 + `verify-codegen` 绿 + 二进制能启动,四重绿之后仍一个请求都服务不了。**
   三个编译器看不见的缺陷:OpenAPI v3 config 没设、自有类型的 openapi 定义没喂进去、
   REST storage 构造的 error 被 `_` 丢掉 ⇒ typed-nil ⇒ segfault。**每一层绿都要各自的证据。**
2. **`verify-codegen` 曾经通过且毫无意义** —— 生成器按二进制名缓存(实际用着 1.24 的生成器)、
   `|| true` 吞掉硬失败。两处都修了,并验证过它会报红。
3. **kubezoo 的 fork 基线比 1.24 还老**,所以多数"冲突"是基线错位造成的假冲突。
   一处有意与上游分道:CRD terminating 时拒绝 create ——上游改用 wrapper 包 admission 链,
   而 kubezoo 传的是 nil 链,保留直接拒绝(文件里有注释)。


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
- [x] ✅ **`-A` 按 namespace 扇出 —— LIST 部分已实现**(`pkg/proxy/fanout.go`,设计 `docs/design-list-fanout-cn.md`)
      ⚠️ **本条原先写着"卡在分页 / resourceVersion 语义让步,需要产品拍板" —— 那是错的。**
      原生 `continue` token 里就带着 revision,store 强制每页同一 revision ⇒ 分页 LIST 本来
      就是一个快照。扇出用 `resourceVersionMatch=Exact` 把所有子 LIST 钉在同一个 R 上即可,
      **没有语义让步**;成本还更小(watch cache 按 namespace 建索引 + `ListFromCacheSnapshot`
      1.34 起默认开)。
      实现三坑(全实测):`continue` 与 `resourceVersionMatch` **互斥**(续读只传 token);
      游标坏掉是**翻不完**而不是少返回 ⇒ 守卫必须带超时;自己的 token 必须与上游区分,
      不认识的一律拒而**不是透传**。
      守卫 `verify.sh` 三条,已验证弄坏游标会红
- [ ] **下一步:WATCH 多路复用**(`-A` 的 watch 扇成 N 条流,对客户端合成一条)。
      现在能干净地从 LIST 返回的 R 起步。⭐ **租户自装 operator 的必需件**
      (cluster-wide informer 走的就是这条路)
- [ ] **DaemonSet 未在代理层拒绝** —— FAQ 称限制,但 `apigroups.go` 正常注册代理。由 Kyverno 策略补(见 3.2)
- [x] ✅ **kubezoo 现在能认 in-cluster ServiceAccount token**(审计 §Y,已实测)
      原因是从未设置 `ServiceAccountTokenGetter`(1.24 fork 时代的遗留:那时非绑定 token 还在)。
      修法:`cmd/kubezoo/app/tokengetter.go` 用已有的上游客户端实现,在
      `applyAuthenticationOptions` 接上。⚠️ 不能用上游的 `NewGetterFromClient`(无条件解引用 lister);
      ⚠️ 必须走原生上游客户端,不能走转换层。
      ⚠️ **配置前提:`--service-account-issuer`/`--api-audiences` 必须等于上游的**,
      `up.sh` 已改成从上游读。守卫 `verify.sh` 第 22/23 条,已验证会红。
      **实测**:Pod 内用 SA token 打 kubezoo → `/apis/example.com/v1` 200、建 Widget 201、
      上游落地 `widgets.111111-example.com`

- [x] ✅ **四个缺口逐条重测完毕**(审计 §Z)。手段:mutate 注入 `KUBERNETES_SERVICE_HOST`
      指向 kubezoo(`config/policy/tenant-api-endpoint.yaml`,podspec env 压过 kubelet 注入的)。
      §X③ operator 组名错位 → **解决**;**每租户不同 operator 版本** → **成立**(实测:同名组
      `example.com`,111111 只见 v1、222222 只见 v2);§T 冻结绕过 → **关上**(SA 写 403);
      §Q/§S binding 绕过 → **关上**(被 VAP 拒)。
      ⚠️⚠️ **部署前提**:kubezoo 的服务证书**必须由 Pod 信任的那个 CA(上游集群 CA)签发**,
      否则所有 in-cluster 客户端 TLS 失败;`--service-account-issuer`/`--api-audiences` 必须等于上游的
- [ ] **代价:kubezoo 进入租户全部工作负载的 API 路径** ⇒ #84/#85 变成核心议题。
      已定:用 kubegateway 挡在前面做精准管控
- [ ] `config/policy/tenant-api-endpoint.yaml` 里的地址是**占位符**,部署时必须换成
      kubezoo 在集群内的可达地址(Service ClusterIP / DNS 名)

- [ ] ⛔ **P0 剩余:租户自装 operator 还差 ClusterRole 这一关**(审计 §X②,§Z 已解掉另外两条)
      `helm --create-namespace` 不工作 —— **根因已查明**(审计 §AA):helm 先检查资源
      再建 ns,而租户在不存在的 ns 里 `GET` 得到 **Forbidden**(上游 admin 得到 NotFound),
      helm 当致命错误中止;**根本没发出建 namespace 的请求**。
      ✅ **已修**(`proxy.shapeError`,守卫 `verify.sh` 两条)⇒ helm 现在会建 namespace 了。
      ⛔ 但第二个坎关不掉:**上游授权器缓存延迟**(RoleBinding 出现 169ms / 真能写 312ms),
      helm 紧接着写 release secret 仍失败 ⇒ **失败一次、重试即成功**。
      试过在建 ns 时同步下发 RoleBinding —— **循环失败**(请求以租户身份发出,而租户
      在新 ns 里正好还没权限),且即便绕过也仍有那 143ms,已撤回;
      **任何带 ClusterRole 的 chart 仍装不上** —— 租户集群级零权限,RBAC 提权防护拒绝,
      连 `events`/`secrets` 都不行,**走不走 kubezoo 都一样**(检查在上游做);
      ~~operator 的 Pod 看不见自己的组~~ ✅ 已由 §Z 解决。
      待定放开方式:给租户在**自己前缀的组**上授集群级权限 —— 需单独设计
      ⚠️ **这推翻了我上一轮的说法**("生态里其余的租户自己装")——
      能自装的是小子集,**平台托管形态因此更重要而不是更不重要**。
      待定路线:给 operator 挂租户 kubeconfig 指向 kubezoo(架构上成立,**未实测**,
      本轮 lab 里 kubezoo 绑 127.0.0.1,Pod 够不着)

- [x] ✅ **system CRD 共享:FAQ 已是实话;产品决策 = 先不实现**(审计 §W)
      触发重评的条件:**出现第一个真实消费者**(候选:kubetron 网络 CRD)。
      理由:难点不在 kubezoo(它只决定"谁看得见"),而在平台 operator 用平台凭据
      调谐**租户写的 spec** ⇒ confused deputy,**审查必须针对具体 operator**;
      且集群级共享 CR 无 RBAC 兜底。
      ⭐ 顺手修掉那段 `TODO: temporary fix for system crd`:它**不是死代码**(读路径要用),
      但方向传错 ⇒ 租户读回 `111111-example.com/v1`(实测)。已改成只在读方向解析平台 CRD,
      写方向拒绝(既守住 FAQ 的说法,也堵掉存在性预言机)

- [x] ✅ **拒绝消息泄漏扫描**(审计 §V)—— 主结论干净:7 条策略消息**无 `111111-` 前缀**,
      跨租户 CRD 查询无存在性预言。真泄漏只有一处:`admission webhook "validate.kyverno.svc-fail"`
      暴露平台用什么策略引擎 ⇒ 已在 `TrimTenantIDFromStatus` 擦掉,保留可操作部分。
      ⚠️ 方法学:第一遍 grep 报的四条里**三条是租户自己输入的回显**,
      判据是"这条信息租户本来知不知道"

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

### 1.5 租户停机:两种模式(#90)—— 已实现并实测

> 详见架构文档 §10。⚠️ **不要标记为全部完成**:`Frozen` 够不到租户自建的 RoleBinding
> 是**有意留的边界**,所以它**不能单独当取证冻结用**。

- [x] Tenant API 加 `spec.suspension`(`mode: ReadOnly|Frozen` + `reason`),**已能正确落盘**
- [x] **执行逻辑已实现并实测**(两层),详见架构文档 §10:
      前门 `pkg/filters/suspension.go` 按模式拒绝并给出可读文案;
      控制器按模式收窄/撤销上游 RBAC(ReadOnly 换只读 roleRef —— `roleRef` 不可变,
      靠 reconciliation **recreate**;Frozen 删绑定)
      - 实测四段:正常 → ReadOnly → Frozen → 解除,**Pod 全程 Running / restarts=0**,
        解除后 roleRef 自动恢复成 admin、租户可写
      - 两种模式都保留 discovery,否则 kubectl 在构造请求阶段就挂,租户看到的是"客户端坏了"
      - ReadOnly 放行 `authorization.k8s.io` 的 review,拒绝 `exec`/`attach`/`portforward` 与 `serviceaccounts/token`
- [x] **`Revoked` 改名 `Frozen`**:"revoke" 在安全语境指**吊销凭据**,而租户证书**照样能通过认证**,
      被拒是在授权层;且"吊销"含不可逆意味,这个状态**设计上可逆**。"冻结"自带
      **可解冻 + 资产保全**两层意思,正是两个场景共同的定义性属性。
      ⚠️ 主动挡掉的歧义:**冻结的是租户的操作能力,不是负载** —— k8s 里 `Job.spec.suspend` 是停负载,这里 Pod 继续跑
- [x] ⭐ **改名时测出真缺陷:mode 根本没有校验**。写 `Revoked` 照样存进去,然后
      **两层各读各的**:前门落到 read-only 分支(拒写、放读、文案还解释不了原因),
      控制器却把上游 RBAC 留在**完整 admin**。一个笔误就能造出"半停机"。
      修:① `Validate`/`ValidateUpdate` 拒绝非法 mode(实测 `Revoked` 现在被拒并列出合法值);
      ② 两层的运行期兜底统一为**认不出就按最严处理**(存量对象用),而不是最松
- [x] **Frozen 够不到租户自建的 RoleBinding —— 有意不做**(决定已下)。
      实测坐实:租户把 `admin` 绑给自己的 SA,冻结后该 SA 仍 `can-i delete pods = yes`。
      不补的理由:**控制面冻结从来管不到容器里已在跑的代码** —— 租户可以预埋 dead-man switch,
      换 VM/kata 也堵不住;而 k8s 对象层的那半,正解是**冻结时做快照**而不是把冻结做严
      (有快照之后,SA 再删本身就是篡改证据)。控制器仍每次列出这些绑定,
      但那是**陈述边界供运维决策**,不是待办
- ⛔ **冻结时快照对象清单 —— 不在本机制内做**(决定已下)。
      取证确实需要它,但那是**快照语义**的事:一份快照要能说清"是哪个时间点/哪个 revision 的视图"、
      怎么存、留多久、谁能读。停机机制给不了这些保证,硬塞进来只会让它背上一个兑现不了的承诺。
      **停机只管控制面**;快照留给未来的快照语义路径统一做
      (⚠️ 那条路径本身尚未设计,不要当成已规划)
- ⛔ **节点级硬冻(`cgroup freezer`)—— 不是 kubezoo 的工作**(决定已下)。
      kubezoo 只交付"控制面冻结"这个**可被编排层操作的原语**;要不要连数据面一起冻、
      节点上跑什么 agent、取证流程怎么走,都是**上层编排的决策**。
      手段本身记在架构文档里供编排层参考(cgroup v2 `cgroup.freeze=1` / v1 `freezer.state=FROZEN`,
      冻内核调度不发信号,进程不退出、内存完整),
      连带那个坑也记了:**冻住后 liveness 探针会失败 → 到阈值重启容器,反而毁现场**,
      要先摘探针再冻(未实测)
---

### 1.6 ⚠️ 撤回的一条"P0" —— 以及它换来的真实教训

我一度记录了一条 P0:"租户新建的 namespace 永远拿不到 RoleBinding,永久不可用"。
**这条是错的,已撤回。**

**干净 lab 里逐点验证,整条链路是通的**(加临时日志确认):

```
租户 create ns probe-ns
  → DIAG enqueueNamespaceOwner ns=111111-probe-ns tenant=111111   ← informer 投递了
  → DIAG processNextItem got {tenantId:111111 eventType:1}         ← worker 处理了
  → 111111-probe-ns 的 RoleBinding = 1                             ← 下发了
删掉某个 namespace 的 RoleBinding → 下一次 Update 立刻补回来        ← 收敛也是好的
```

**当初为什么会看错**:测到一半我在后台重跑了一次 `up.sh`,它会重建 PKI 并重启整个栈。
那之后的每一次观测,都是打在一个**被我自己搅动过、状态不明**的 lab 上。

⭐ **这是本次会话第四次同一形状的失误**(前三次:二进制没重新编译就归因、
`jsonpath` 读一个不存在的对象、`wc -l` 把 "No resources found" 数成 1)。
**共同点:环境没有先自证,就开始解释现象。**

> **规矩**:测出异常时,第一步不是解释,是**确认被测环境就是你以为的那个** ——
> 二进制是不是新的、对象是不是真存在、跑的进程是不是那一个。

### 1.7 ✅ 顺手修掉一个真缺陷(读码发现,与上面那次误报无关)

租户 informer 的三个 handler **共用同一个 `Event` 变量和同一个 `err`**,
逐字段赋值后再入队。informer 从自己的 goroutine 调 handler,所以两个事件并发时
可以在"写 tenantId"和"写 eventType"之间交错,**入队一个张冠李戴的事件** ——
比如一个 create 被当成 delete 处理。改成每个 handler 各自构造事件。

## 阶段 2:隔离正确性审计(#82)✅ 主体完成

> ⭐ **报告是 [`docs/isolation-audit-cn.md`](docs/isolation-audit-cn.md)** —— 逐条 findings、
> 实测记录与负向对照都在那里,**那份是权威版本,不要在这里复述**。
> 安全边界的总览见 [`docs/security-admission.md`](docs/security-admission.md)。

findings A–M 共 13 条,除下列一条外全部已修并实测(I 与 DaemonSet 由**策略层**执行,
策略在 `config/policy/`,lab 默认装 Kyverno 并应用):

- [ ] **`-A` 与 cluster-scoped 请求** —— #87 之后对租户直接 Forbidden。
      改成"逐 namespace 扇出合并"要先定两个语义让步,见 §4.1 与架构文档 §11.5:
      **分页**(kubectl 默认就带 `--chunk-size=500`)与 **resourceVersion**
      (跨 namespace 没有单一快照,而旧行为是有的)

> ⚠️ 每条测试**必须带负向对照**(确认测试真的走到了被测分支)—— 本项目在这上面栽过四次:
> 脚本参数错位、前一步删过 Pod 导致用量归零、`nodeName` 造假让 Pod 被 GC、
> 客户端缓存让服务端修复看着没生效。**每次都"看起来得出了结论"。**

> lab 需求:一套独立 kind 集群、独立端口,**不得影响 `kubebrain-dbaas-control-plane`**。

---


## 阶段 3:平台层

### 3.1 策略层(选型与判据见架构文档 §8)

> ⚠️ **归属判据见架构文档 §8.0**:准入只有写路径、碰不到响应 ⇒ 需要租户**看到**翻译后视图的
> 事策略层做不了;只在写路径且**换个平台会变**的才归这里。
> ⭐ 能用 CEL 表达的优先 **MAP/VAP(进程内)**,不必是 webhook。

- [x] ✅ **策略验证套件 `hack/lab/verify.sh`**(21 条,已验证摘掉策略会红)。
      ⛔ 第一次跑就抓到我自己引入的 P0:`tenant-frozen-deny-writes` 少了
      `scope: Namespaced` ⇒ 套到全集群每一次集群级写入 ⇒ **Kyverno 注册不了自己的
      webhook** ⇒ 三条策略永不就绪、`pods` webhook 根本没注册 ⇒ 租户 hostNetwork/
      hostPID/nodeName 全放行。症状只有 `READY=<none>`。详见审计 §U
- [ ] ⭐⭐ **铁律:所有策略匹配一律反向写**(`exclude` 平台自身 namespace,匹配其余全部)。
      **禁止用正向 selector 选租户 namespace 的标签** —— 租户能编辑自己建的 namespace,
      摘掉标签即**绕过整套策略**,此时他仍有全权建 Pod ⇒ B1 隔离前提全部落空
      - 同族问题仓库里已出现一次:配额 webhook 的 objectSelector(见 2.3)。
        **通用规则:排除条件只能建立在租户无法控制的东西上**
- [ ] ⭐ **优先 MAP/VAP(进程内)而不是 webhook** —— 见架构文档 §8.0。
      代价:MAP **没有 autogen**,PodSpec 那 8~9 个 kind 要逐个手写路径(`CronJob` 多一层)
- [ ] 必备策略(**都得策略引擎做**;原生 PSA 连它自己那一条都守不住,见下):
      - [x] ✅ **三个 class 字段由平台决定** —— `runtimeClassName` / `ingressClassName` /
            `priorityClassName`(含 `spec.priority`)。`config/policy/tenant-platform-classes.yaml`,
            已在 7 个租户 namespace 上实测;kubezoo 侧实现已删除
            ⚠️ **本条原先的理由是错的**:曾写"租户写 `kata` 会被改写成 `<tid>-kata` 而不存在,
            所以只能由平台注入" —— **该字段根本不被改写**,#82 实测租户写什么就原样生效。
            真实情况不是"租户用不了",而是**租户能引用平台的任意一个**
            - 三个坑:PodSpec 嵌 **9** 个 kind(Kyverno autogen 只覆盖 8,缺 `PodTemplate`;
              MAP 没有 autogen,`CronJob` 多一层)、`spec.priority` 要跟名字一起清、
              废弃的 `kubernetes.io/ingress.class` 注解要跟字段一起删
      - [x] ✅ **拒绝 `spec.nodeName`** + **白名单 `tolerations`** ——
            `config/policy/tenant-scheduling.yaml`,已实测(审计 §O)。
            ⚠️ 三个坑:`tolerations` 不能一刀切(`DefaultTolerationSeconds` 是进程内插件,
            在 webhook 之前就加了两条,实测确认)、规则只能匹配 `CREATE`(否则已调度的
            Pod 再也改不动)、多策略并存时**判据是拒绝消息里的策略名**
      - [x] ✅ **PSA `restricted` 等价规则** —— `config/policy/tenant-pod-security.yaml`,已实测。
            ⛔ **原生 PSA 在这里是废的**:PSA 判定输入是 namespace 标签,而 kubezoo 只钉死
            `kubezoo.io/tenant`,其余标签原样转发 ⇒ 租户把自己标成 `privileged`
            (建时带 / 事后 patch 两条路)就拿到 **Running 的 privileged + hostNetwork Pod**,
            即使全局默认已是 `restricted`。**又是"判定条件建在租户可控输入上"那个形状。**
            修法用 Kyverno `validate.podSecurity`(按 `kubezoo.io/tenant` 匹配,且**有 autogen**),
            并把 PSA 标签钉回 `restricted` 让原生 PSA 反过来兜底。详见审计 §N
      - [x] ✅ **落点控制:每租户节点池 + 注入替换 —— 已实测可行**(审计 §R)
            `config/policy/tenant-placement.yaml`;⚠️ 两个承重前提见下,残余=binding 在 API 层仍成功
            > **一句话原则(用户定案):租户看不到节点,没有任何调度权,他写的东西会被平台替换掉。**
            **完成判据 = 租户不能通过任何一条写入路径影响落点**,不只是 PodSpec 字段 ——
            `pods/binding` 是 Pod 建好之后的**另一次写入**,替换够不到它(§Q)
            - 每租户有自己的 worker 节点池,**节点带污点**防普通应用调度过来
            - 平台**替换**租户 Pod 的落点字段:注入该池 `nodeSelector` + `toleration`
              + `topologySpreadConstraints`(**不是拒绝**)
            - ⭐⭐ **注入的 `nodeSelector` 是承重件**:binding 那条路上 kubelet 不检污点
              但**检 nodeSelector** ⇒ 它是跨租户 binding 的唯一拦阻。
              **前提是那个标签只有该租户节点才有**,用共有标签等于没兜住
            - ⚠️ 三坑(**全部实测过**):必须整体覆盖不能 merge;覆盖会删掉
              `not-ready`/`unreachable` 两条(注入的那份已带上);
              **`restrict-tolerations` 与注入冲突已复现并删除该规则** ——
              通用教训:注入型策略上线时必须清掉同字段的验证型策略
            - [x] ✅ **binding 在 API 层也堵住了 —— 但只能用原生 VAP**:
              Kyverno 3.8.2 的 `kinds: [Pod/binding]` 子资源匹配**实测不生效**
              (Ready + webhook 注册了 + 日志里连请求都没有,第四次"Ready 但什么都不做")。
              `config/policy/tenant-deny-binding.yaml` 用 `ValidatingAdmissionPolicy`。
              ⚠️ 表达式写"只放行 system:kube-scheduler",无条件拒绝会让所有 Pod 永远 Pending
            - ⛔ **打散不要用 required podAntiAffinity**:每评估一个节点扫一遍已有 Pod,
              是调度吞吐杀手,按北极星(规模优先)大集群上会先撞这堵墙 ⇒ 用 `topologySpreadConstraints`
            - ⛔ **跨租户共驻 affinity 表达不了**(笛卡尔积)⇒ 只能靠节点池:污点 + 注入
            - ⚠️ **纠正一个直觉**:"租户看不到 Node ⇒ 他设 nodeSelector/tolerations 无效" **不成立**。
              评估这些字段的是**调度器**,在上游用自己的凭据读真实 Node,不查租户可见性;
              代码确认 `pkg/` 里一处都没碰过这几个字段。且 `nodeSelector` 匹配的是**标签**,
              标准标签全世界一样,不需要知道节点名
            - ⚠️ 打污点 / 划分节点标签属于**平台基础设施**,不是 kubezoo 也不是策略层
      - [ ] 等前置决策:强制 `schedulerName` —— 只有平台真跑了**承载策略的**自定义调度器时
            才是控制点(租户填回 `default-scheduler` 即绕过)。B1 的 kata 节点池方案未定,
            **现在不算未决项**
      - [x] ✅ 拒绝 DaemonSet —— `config/policy/tenant-deny-daemonset.yaml`,已实测
- [x] ✅ **`pods/binding` 已坐实并已堵住**(审计 §Q/§R⑤/§S)
      推论的每一步都对:租户 `*` on `*` 含 `create pods/binding`;`deny-nodename` 匹配
      `kinds: [Pod]` 匹配不到 Binding;kubelet 只检 `NoExecute`,`NoSchedule` 被绕开。
      ⛔ **Kyverno 3.8.2 的 `kinds: [Pod/binding]` 子资源匹配实测不生效**
      (Ready + webhook 注册了 + 日志里连请求都没有)⇒ 改用原生 VAP
      `config/policy/tenant-deny-binding.yaml`,两条路(kubectl / Pod 内直连上游)都已验证被拒

- [x] ✅ **节点名从 `spec.nodeName` 漏给租户 —— 定案接受,不改**(用户 2026-07-29)
      知道节点名只在能拿它做事时才值钱,而落点字段全被平台替换 ⇒ 名字兑现不了;
      藏掉则 `-o wide`/`describe` 失真,代价确定收益为零。
      ⚠️ **前提**:替换要覆盖到 `pods/binding`(它是直接写节点名的另一次写入),
      而兜住它的是 kubelet 核对的那个注入 nodeSelector ⇒
      **"注入标签每租户专属"是本定案的前提,不是优化项**。详见审计 §P

- [x] ✅ **#90 `Frozen` 现在够得到租户预置的 ServiceAccount 了**(审计 §T,已复测)
      两半:控制器冻结时给租户每个 ns 打 `kubezoo.io/frozen`(`markFrozen`,守卫测试已验证会红)
      + 上游 VAP `config/policy/tenant-frozen-deny-writes.yaml`。
      ⭐ 表达式是**放行不属于本租户的身份**,不是"拒绝租户" —— 否则 controller-manager
      一起被拦,症状是租户 Deployment 永远不出 Pod 且无报错指向策略(复测 ⑤ 专门验这条)。
      ⚠️ 只拦写不拦读:冻结的承诺是什么都不删、负载照常跑

- [x] ✅ **"让 kubezoo 拦 Kyverno 拦不住的" —— 已否决(实测,审计 §S)**
      租户 Pod 用 SA token **直连上游,完全不经过 kubezoo**(实测 binding HTTP 201 成功)。
      ⇒ **写路径的强制不能放在 kubezoo**,退路是 VAP/MAP 或 RBAC。判据已补进架构 §8.0

- [ ] 用 `generate` + `synchronize: true` 承接 namespace 配套对象(租户删了自动重建):
      per-namespace RoleBinding(1.1)、`kubetron-network` ConfigMap(3.2)、PSA 标签、
      ResourceQuota/LimitRange
- [ ] `failurePolicy` 定为 **`Fail` + 多副本 + PDB**(`Ignore` 的失效是静默的,直接击穿隔离前提)
      - ⭐ **但这个两难可以绕开**:`MutatingAdmissionPolicy` 在 **1.36 已 GA 且默认开**,
        跑在 apiserver 进程内,**根本没有 failurePolicy 这一说**。凡能用 CEL 表达的走 MAP/VAP
      - ⚠️ 若确实用 Kyverno:`forceFailurePolicyIgnore` 环境变量能**一次性把所有策略变成 Ignore**
        (`pkg/toggle/toggle.go:24`)。必须锁死并纳入巡检,否则 `Fail` 只是纸面上的
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

## 诚实边界(架构文档 §15 的摘要)

架构判断建立在**源码阅读 + 各项目自身的测试报告**上,不是端到端实测:

- **kubezoo + kubetron 这个组合从未运行过**;3.2 的三处接缝均为纸上推演
- kubetron 的数据(e2e 76/76、SVC 13/13、kata-fc 5.4s Ready、Knative 冷启动 5.3s)
  是其自测结果,非本项目测量,亦非生产验证
- 阶段 2 配额那三条、Kyverno 拒绝消息泄露面,均未做运行时复现

准确表述:**未发现结构性障碍,且工程量比 B2 小一个量级** —— 而非"已验证可行"。
