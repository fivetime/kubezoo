# 出站连接审计:哪个租户可写字段被平台组件变成一次向外的连接

与 `PersistentVolumeClaimSpec`、`PodSpec`、`ServiceSpec` 三轮**不是同一套做法**,这是本轮的全部意义。

前六轮问的是「这个字段由**哪一层**守」。这轮问的是另一个问题:

> **哪个租户可写的字段,会被某个平台组件变成一次向外的连接?**

---

## 0. 起点:这条轴上**没有第二道防线**

连接发起于**控制面的网络命名空间**或**平台节点上**,永远不在租户自己的网络里。于是:

| 通常兜底的东西 | 在这条轴上 |
|---|---|
| 租户的 NetworkPolicy | ❌ 无关 —— 流量不从租户 Pod 出去 |
| OVN ACL / kubetron 逻辑交换机 | ❌ 无关 —— 同上 |
| OpenStack 安全组 | ❌ 无关 —— 那管的是租户自己的 port |
| 上游 RBAC | ❌ 无关 —— 拨号的是平台组件,用的是它自己的身份 |

⇒ **写入时拒绝是唯一能拒的地方。** 这就是为什么一个字段在「谁守它」这条轴上完全无害,
在这条轴上却可能是致命的。`type: ExternalName` 正是如此:ServiceSpec 那轮把它判成
「低 —— CNAME 别名,只影响解析到该租户 namespace 里这个名字的客户端」
(`docs/service-spec-audit-cn.md:44`),因为那轮看的是**租户自己的流量**。
而上游的 `ResolveCluster` 会把它解析成 `https://<externalName>:<port>`,
于是被 `pkg/convert/webhookconfiguration.go` 明确拒掉的 `clientConfig.url` 从
`clientConfig.service` 这条**批准过的**路重新长了出来。

⭐ **判定门槛(严格执行)。** 不是每一次租户可控的拨号都是缺陷。kubelet 去拉一个租户点名的
公开镜像,那就是「跑一个容器」这件事本身。必须命中下面至少一条才算发现:

- **(a) REACH** —— 连接发起于租户自己够不到的地方(控制面 netns、平台节点、管理网),
  于是那个组件成了一个通向租户被隔离在外的网络的跳板;
- **(b) CREDENTIAL** —— 连接带着租户手里没有的东西:平台 token、客户端证书、
  ServiceAccount、别人对象的 AdmissionReview;
- **(c) VOLUME** —— 频率或数量由租户决定,平台组件成了放大器。

三条都不命中的,**不是发现**,并且要指名它凭什么可以接受。

---

## 1. 判定表

| 租户字段 | 谁去拨号 | 从哪个网络 | 带着什么 | 判定 |
|---|---|---|---|---|
| **`pods/status` 的 `status.podIP` / `status.podIPs[].ip`** | kube-apiserver 的 `pods/proxy`(`pkg/registry/core/pod/strategy.go:625`)与 `services/proxy` | **控制面 netns** | 无客户端证书(`InsecureSkipVerify: true`),但响应体原样回给租户 | ⛔⛔ **未守且可达** → §2① |
| **探针与生命周期钩子的 `.host`**<br>`{liveness,readiness,startup}Probe.{httpGet,tcpSocket}.host`、`lifecycle.{postStart,preStop}.httpGet.host`,外加 `httpGet.httpHeaders[]` | kubelet(`pkg/kubelet/prober/prober.go:162/188`、`pkg/kubelet/lifecycle/handlers.go:125`) | **平台节点 netns = 管理网** | 租户自选的 `Host` 与 `Authorization`;**没有 `IsGlobalUnicast` 过滤**,`169.254.169.254` 在射程内 | ⛔⛔ **未守且可达** → §2② |
| **Ingress `metadata.annotations`**<br>`auth-url` / `mirror-target` / `server-alias` | 平台的 ingress 控制器 | 平台节点 + **唯一接公网的那个位置** | 每个入站请求一次子请求;`auth-response-headers` 把选中的响应头回读 | ⛔ **未守**(条件见 §2③) |
| **凭据 Secret 里的 `authURL`** | kubetron `pkg/service/client.go:58`、`pkg/neutron/provider.go:169` | 平台管理网 | 租户自己的 OpenStack 应用凭据 | ⛔ **未守** —— 目的地那一半已记在 `docs/kubetron-knaas-requirements-cn.md` T5;本轮新增的是**时长**,见 §2④ |
| `Service` `type: ExternalName` | apiserver 的 `ResolveCluster` | 控制面 netns | **别人对象的 AdmissionReview** | ✅ **已守** `refuseExternalNameService`(`pkg/proxy/proxy.go:2009`,三条写路径俱全:`:497` / `:735` / `:1888`) |
| `EndpointSlice` / `Endpoints` 的 `addresses[]` | ingress 控制器、metrics 抓取器、`services/proxy` | 平台节点 | 抓取器带 bearer token | ⚠️ **已守,但锚点是租户可写的** → §2① 的后半 |
| webhook `clientConfig.url`(Validating/Mutating) | apiserver | 控制面 netns | AdmissionReview | ✅ **已守** `pkg/convert/webhookconfiguration.go` |
| CRD `spec.conversion.webhook.clientConfig.url` | apiserver | 控制面 netns | ConversionReview | ✅ **已守** `pkg/convert/customresourcedefinition.go` |
| `Service` `externalIPs` / `type: NodePort` / `ports[].nodePort` | 主网络的 Service 代理 | 节点 netns | —— | ✅ **已守** `refuseNewExternalIPs`(`proxy.go:1237`)、`refuseNodePorts`(`:1294`) |
| 内联 `csi` 卷 / 通用临时卷模板 | kubelet 侧的 CSI 驱动 | 节点 | 驱动自己的凭据 | ✅ **已守** `refuseInlineCSIVolume`(`pkg/proxy/inlinecsi.go:54`)、`refuseUnpublishedEphemeralClasses` |
| `apiservices` | 聚合层 | 控制面 netns | 平台身份 | ✅ **不服务** —— kubezoo 根本不注册这个资源 |
| `NetworkPortClaim.spec.credentialSecretRef` | kubetron claim 控制器 | 管理网 | 租户凭据 | ✅ **不可达** —— `cmd/kubezoo/app/sharedcrd.go:45` 只共享 `snapshot.storage.k8s.io`,`kubetron.network.kubevirt.io` 对租户根本不解析。⚠️ **需求文档 T3 写错了**,见 §3 |
| `spec.containers[].image` | kubelet | 节点 | **取决于节点上的 credential provider** | **不是发现**(默认下)—— 拉一个租户点名的镜像就是「跑容器」的定义;残留能力只是一个粗粒度的 `ErrImagePull` 探测口子,严格弱于 §2②。⚠️ 有一个前提没确立,见 §3 |
| `Service` 注解 `lb-cred` / `credential-secret` | kubetron 删除路径(`pkg/service/reconciler.go:510`) | 管理网 | 同 namespace 的 Secret | **不是发现** —— Secret 从 `svc.Namespace` 读(`:526`),租户本来就写得了里面的 `authURL`,**没有新增能力**。但它让 R3 的整改不完整,见 §2④ |
| `IngressClass` 的 `is-default-class` 注解 | **没有人拨号** | —— | —— | **不是本轴的发现** —— 它让平台控制器**停止**接管,方向相反。⚠️ 但在「谁守这个字段」那条轴上它是真的,见 §4 |
| `Ingress` `spec.rules[].host` / `spec.tls[].hosts` | ingress 控制器**接受入站** | —— | —— | ✅ **已守** `config/policy/tenant-ingress-hostnames.yaml:54` 与 `:72`。⚠️ 但它只读这两张表,`server-alias` 是第三张,见 §2③ |

---

## 2. 结论

### ⛔⛔ ① `pods/status`:租户伪造 `status.podIPs`,apiserver 就去拨它 —— 而已经上线的 EndpointSlice 守卫锚在同一个字段上

**这是本轮最重的一条,因为它同时是一个新洞和一个已有守卫的地基问题。**

`pods/status` 是被服务的可写子资源(`cmd/kubezoo/app/apigroups.go:75`),
`pods/proxy` 也被服务(`:124`,由 `pkg/proxy/pod/proxy.go` 原样转发,只改 `namespaces/<ns>` 那一段路径)。

**没有任何一层看 pod status:**

- kubezoo:`tenantProxy.Update` 的整条 refuse 链(`pkg/proxy/proxy.go:494-517`)八条守卫,
  **没有一条读 status**,然后 `:593` 直接 `client.UpdateStatus`。PATCH 走的
  `guaranteedUpdate`(`:1881-1910`)同样。全仓 `grep PodIP` 在 `pkg/` 和 `cmd/` 里
  **只有一处非生成码命中**,就是 `pkg/proxy/endpointaddress.go:143`,而它是**读**。
  `pkg/convert/` 里没有 `pod.go`,Pod 走 `DefaultConvertor`,status 原样透传。
- 策略层:`config/policy/` 十份文件,`kinds:` 全是 `[Pod]` / 工作负载 / `Namespace` / `Ingress`,
  **没有一条匹配 status 子资源**。⚠️ 而且 `tenant-deny-binding.yaml:6` 自己记着
  「Kyverno 用 `kinds: [Pod/binding]` 匹配子资源**不生效**」—— 所以就算想用 Kyverno 补,也补不上。
- 上游:`validatePodIPs`(`pkg/apis/core/validation/validation.go`)只查 IP 语法、
  双栈配对、以及 `podIP == podIPs[0].ip`。`podStatusStrategy.PrepareForUpdate`
  重置 spec、deletionTimestamp、ownerReferences —— **不重置 status**。
  `noderestriction` 准入插件认的是 `system:nodes`,对普通用户不生效。
- RBAC:租户在自己 namespace 里是 `*` on `*`(`kubezoo-controller/pkg/controller/rbac.go:252`),
  RBAC 的 `Resources: ["*"]` 匹配子资源;`NotGrantedToTenants`
  (`kubezoo-contract/pkg/common/clusterscope.go:172`)只拒 `nodes/proxy`、`nodes/status`、
  `namespaces/status|finalize` —— **不拒 `pods/status`**。

**能拿到什么。** `pkg/registry/core/pod/strategy.go:573-581` 的 `getPodIP` 返回
`pod.Status.PodIPs[0].IP`,`ResourceLocation` 唯一的过滤是 `:607` 的 `!ip.IsGlobalUnicast()`
—— 那只拒 loopback / link-local / 组播,**每一个 RFC1918 地址和每一个公网地址都放行**;
scheme 和端口来自请求路径(`SplitSchemeNamePort`,`:586`),`loc.Host` 在 `:625` 拼好。
于是一次 `GET /api/v1/namespaces/<ns>/pods/https:<name>:2379/proxy/` 就让 apiserver
**从控制面的网络命名空间**去连 KubeBrain/etcd、PD/TiKV、OVN/OpenStack 的管理端点或任何一台
节点的 kubelet 端口,并把完整响应体交回租户。Pod **既不需要被调度,也不需要 Running**。

⭐ **而这不只是一个新洞,它落在一个已经上线的守卫的地基上。**
`refuseForgedEndpointAddress`(确认实例 #3)的注释把话说得很准:
「services/proxy 已经拒了这个,在 `isValidAddress` 里 …… **这是整棵树里唯一一处针对这个字段的检查**」
(`pkg/proxy/endpointaddress.go:60-67`)。它说对了 —— 但那个检查的**锚点是租户自己写得动的**:
`endpointaddress.go:143` 读的是被引用 Pod 的 `status.podIPs`,上游的 `isValidAddress`
(`pkg/registry/core/service/storage/storage.go:483-487`)读的是**同一个字段**。
**先写 pod status,再写 EndpointSlice 地址,守卫和上游会一起为伪造背书。**

⛔ **同一个守卫上还有第二个、互相独立的洞**,`endpointaddress.go:154-157`:

```go
// A pod with no IP yet is not a forgery; it is a race with the scheduler.
if len(ips) == 0 {
    return nil
}
```

注释把它描述成「与调度器的竞态」。对一个**永远不会被调度的 Pod**(未绑定 PVC、
无法满足的拓扑约束、单机放不下但配额内的 request —— ⚠️ 别用「超大 request」,那可能先撞
租户自己的 ResourceQuota),这个窗口**永不关闭**。于是连 status 都不用写:
只要 `targetRef` 指向自己一个 Pending 的 Pod,任意地址就能过。

⚠️ **判定归类要诚实:这是 (a) REACH,不是 (b) CREDENTIAL。**
`CreateProxyTransport`(`pkg/controlplane/apiserver/config.go:433-442`)构造的是
`tls.Config{InsecureSkipVerify: true}`,**不带客户端证书**。租户拿到的是「到达能力」+
一个未验证 TLS 的响应体,不是平台身份。⭐ 而「到达能力」这一条在本平台是**明文承重**的:
`docs/operations-cn.md:294-312` —— 「租户网络必须够不到上游 apiserver —— 这条是承重前提」。
`pods/exec`、`pods/log` 都在租户自己的 netns 里,给不出等价物。

#### 修复

**`refusePodStatusIPs`,三条写路径全上。** 不需要 `createOnly` 条目:
上游的 `podStrategy.PrepareForCreate` 本来就会把 status 清成 `Pending`,
所以在 Create 上跑是零代价,而 `TestEveryWritePathRunsTheSameGuards` 要的正是对称。
放进 `Update`(`proxy.go:494-517`)、`Create`(`:709-761`)、`guaranteedUpdate`(`:1881-1910`)三处。

规则是**只拒新增**,不是拒非空 —— 与 `refuseNewExternalIPs` 同形:比对 `original`,
租户保留或删掉已有的 IP 放行,**加一个新的**拒掉。理由和 `externalIPs` 那条一模一样
(`docs/service-spec-audit-cn.md:77`):否则一个已经带着 IP 的 Pod,它的所有者连改标签都改不动。

⚠️ **但这里比 `externalIPs` 好办**:kubelet **从不经过 kubezoo**,所以在正常运行中
**没有任何合法写入者会通过 kubezoo 往 pod status 里写 IP**。存量 Pod 的 IP 是 kubelet
直接写给上游的,租户经 kubezoo 读得到、也能原样回写。⇒ 子集规则是纯保险,不是妥协。

⛔ **`endpointaddress.go:154-157` 必须同时改**,否则修了一半:把「无 IP 即放行」
改成「无 IP 即拒绝」。这会真的动到调度竞态那个场景 —— 代价是租户在 Pod 拿到 IP 之前
写不了指向它的 EndpointSlice,重试即可;收益是那个永不关闭的窗口消失。
⚠️ 两处必须一起改:只修 status 不修这里,永不调度的 Pod 那条路原样还在;
只修这里不修 status,伪造 status 那条路原样还在。**这正是本仓最常见的双生分叉 bug。**

---

### ⛔⛔ ② 探针与生命周期钩子的 `.host`:kubelet 从平台节点拨一个租户挑的地址,结果当成 Event 交回来

`spec.containers[].readinessProbe.tcpSocket.host` 这一族字段,规则简单到没有解释余地:

```go
host := p.TCPSocket.Host
if host == "" {
    host = status.PodIP
}
```
（`pkg/kubelet/prober/prober.go:188-190`;HTTP 侧是 `pkg/probe/http/request.go:46-49`
的同一句;生命周期钩子是 `pkg/kubelet/lifecycle/handlers.go:125`。）
**Pod IP 只是兜底,租户写的才是主。**

**四层全空,一层一层查过:**

1. **kubezoo** —— `pkg/proxy` 里 18 个 `refuse*`,没有一个碰探针;`pkg/convert` 里没有探针处理。
   全仓 `Probe|HTTPGet|TCPSocket` 的命中只有 `pkg/apis/openapi/zz_generated.openapi.go`
   和 `cmd/clusterresourcequota/main.go:72`(`HealthProbeBindAddress`,无关)。
2. **策略层** —— `grep -i probe /root/kubezoo-contract/config/policy/` **零命中**。
   `tenant-pod-security.yaml` 钉的是 PSS `restricted`,它关掉了 `hostNetwork`
   (所以租户拿不到节点 netns 的**正路**),但对探针 host 一个字没有。
3. **上游** —— `validateHTTPGetAction`(`pkg/apis/core/validation/validation.go:3562-3586`)
   校验 path / port / scheme / header **名字**,`:3581` 只在 `protocol == HTTP2` 时禁 Host;
   `validateTCPSocketAction`(`:3604`)只校验端口。**Host 从不被检查、不被解析、不被限制成单播。**
4. **kubetron 的改写不是守卫** —— 这条查得最细,因为它看起来最像兜底:
   - `pkg/webhook/probes.go:29-40` 与 `probes_helper.go:33` **都只遍历 `pod.Spec.Containers`**,
     **可重启的 initContainers(sidecar,kubelet 一样会探)两条路径都没走到**;
   - 整个 webhook 由一个**租户自己写得动的 Pod 标签**闸住
     (`objectSelector: kubetron.network.kubevirt.io/enabled: "true"`,`deploy/kubetron.yaml:257-260`),
     **不打这个标签,什么都不会被改写**,而 kubezoo 和策略层都不注入、也不要求这个标签;
   - 双挂载路径 `mutate.go:156-159` **故意跳过探针**;
   - 它真的触发时,是把 host **丢掉**改拨 `127.0.0.1`(`probes.go:59/67/75-79`)——
     那是为了**可达性**(`docs/DESIGN-refactor.md` §3.3),不是为了围堵。

⭐ **对照的兄弟字段:GRPC 探针是安全的**,`prober.go:196` 写死 `host := status.PodIP`,
API 里根本没有 host 字段。**同一个文件里三个分支,两个可控一个不可控** —— 又是双生分叉。

**能拿到什么。** 通不通的回读有三条,而且一条比一条弱地依赖 Event:

1. 传输层错误串(`err.Error()`,`pkg/probe/tcp/tcp.go:56`、`pkg/probe/http/http.go:125`)
   经 `prober.go:124` 进 Warning Event,是完整的**开 / 拒 / 丢包** 端口扫描口子;
2. 3xx 会把**响应体**带回来(`http.go:140`,`Probe terminated redirects, Response body: %v`);
   ⚠️ **普通失败不带响应体** —— `http.go:146` 有一条明写的注释「this user-facing failure
   message must not contain the response body」。别把这一条说过头;
3. ⛔ **就算把 Event 全封了也堵不住**:`tcpSocket` 探针的成败直接翻 Pod 的 `Ready` condition,
   租户从自己 Pod 的 status 上读得到 —— **一个 1 bit 的 oracle**。
   ⇒ **只有写入时拒绝能关掉它。**

判定:**(a) REACH,充分且唯一**。kubetron 自己量过的那句话就是证据:
从节点 netns 够不到 OVN 上的 podIP(`docs/DESIGN-refactor.md:159-163`,「实测超时」)——
那正是探针非改写不可的原因,也就等于说**节点 netns 与租户 netns 是两个不同的可达域**,
而拨号发生在前者。
⚠️ **不是 (b)**:探针不带平台 token、不带客户端证书,User-Agent 是 `kube-probe/…`,
`Authorization` 是租户自己敲的。
⚠️ **(c) 几乎为零,不要写进结论**:`runProbeWithRetries` 的 `maxProbeRetries = 3`
(`prober.go:42`)只在 `err != nil` 时重跑,而 `tcp.go:56` / `http.go:125`
**故意把拨号失败转成 `(Failure, msg, nil)`** —— 所以扫一个关着的端口,那 ×3 根本不触发;
`periodSeconds` 校验只要求非负,0 会被默认成 10s,下限是 1 次/秒/探针,再乘租户的 Pod 配额。
**这是 REACH,不是放大器。**

#### 修复

**照抄 `inlinecsi.go` 的那一刀两半,一个字都不用改形状** —— 因为约束完全一致:
**探针在已存储的 Pod 上是不可变的**,和 `spec.volumes` 一样。

| | 做法 | 依据 |
|---|---|---|
| **模板**(9 种带 pod 模板的 kind) | `refuseProbeHost`,**三条写路径全上** | 模板的字段是**可变的**:创建时被拒的租户,可以先建一个不带探针的 Deployment,再 patch 进去。`proxy.go:529-532` 把这句话写在那里了 |
| **活的 Pod** | `RefusePodProbeHost`,**只在 CREATE 上** | 探针在存储后不可变 ⇒ 在 update 上拒会让本规则之前建的 Pod **连删都删不掉**。与 `refuseTenantChosenNode` 同一个陷阱(`docs/pod-spec-audit-cn.md:193`) |

⇒ `refuseProbeHost` 对 `*core.Pod` 直接 `return nil`(和 `refuseInlineCSIVolume:59-61` 一模一样),
Pod 那半由 `Create` 里显式调用的 `RefusePodProbeHost` 接手(`proxy.go:683` 旁边),
并在 `createOnly` 里写下理由。模板遍历用 `convert.PodSpecOf`,它落到 `podTemplateOf`
(`pkg/convert/placement.go:197-235`),那个 switch **一次覆盖全部 9 种 kind**
—— ⭐ 那条注释说得对:「One switch, deliberately. A second copy of this list is a second
place to forget a kind」。

⛔ **六个位置必须一起动,少一个就是下一个双生分叉:**

1. `{liveness,readiness,startup}Probe.httpGet.host`
2. `{liveness,readiness,startup}Probe.tcpSocket.host`
3. `lifecycle.postStart.httpGet.host`
4. `lifecycle.preStop.httpGet.host`
5. **`spec.initContainers[]` 上的以上全部** —— kubetron 两条改写路径漏的就是这个
6. **`httpGet.httpHeaders[]`** —— 只钉 host 是不够的:
   `pkg/probe/http/request.go:84` 用租户给的 `Host` 头设 `req.Host`,
   `:110-116` 把每个头原样抄进去。留着它,租户仍然在替 kubelet 挑
   `Host` 和 `Authorization`。⚠️ 但 headers 有合法用途(给自己的应用探针加个头),
   ⇒ **只在 `host` 非空时连带拒 headers**,或者只拒 `Host` 这一个头名。

**空 host 是唯一安全的值**,而且不损失任何能力:Pod 自己的 IP 本来就是默认值。
拒绝而不是清空,是因为清空会让一个租户以为自己在探别处、实际在探自己,**静默且难查**;
模板那半可以考虑清空(与 `placement.go` 对模板里 `nodeName` 的处理同形),但要接受这个代价。

---

### ⛔ ③ Ingress 注解:kubezoo 对它**一条策略都没有**,而已有的主机名 VAP 只读三张表里的两张

**kubezoo 侧,读过源码、无条件成立:**

- `pkg/convert/ingress.go:74-87`(Forward)与 `:95-104`(Backward)**只改**
  `spec.ingressClassName` 和废弃的 `kubernetes.io/ingress.class` 注解,**其余注解原样透传**;
- `pkg/proxy` 里没有任何注解允许/拒绝清单,`grep Annotations pkg/proxy/*.go`
  只命中 `watchmux.go:537-557` 的 bookmark 记账;
- `config/policy/tenant-ingress-hostnames.yaml` 只有两条 `validations`:
  `:54` 读 `object.spec.rules[].host`,`:72` 读 `object.spec.tls[].hosts`,
  **没有一条表达式读 `metadata.annotations`**。

⭐ **`server-alias` 是第三张主机名表,而那条 VAP 看不见它。**
那份策略的头注释写着它要防什么:「**任何租户可以声明任何主机名**,由 ingress 控制器按
**创建顺序**裁决 —— 先到先得,**落败的一方零报错**」。
`nginx.ingress.kubernetes.io/server-alias` 从注解里加进同一批 `server_name`,
于是那条策略要防的 §AH 后果**原封不动地从它不读的字段里回来了**。
⚠️ 而且它已经承认自己只解决了一半(「真实客户会要用自己的域名 …… 那一半是产品决策,未做」)
—— 这是**第三**半,而且是不需要产品决策的那半。

⭐ **`auth-url` 与 `clientConfig.url` 是同一个形状。**
`endpointaddress.go:56-60` 自己把这个形状写清楚了:
「ingress 控制器**直连 endpoint IP**,绕过 ClusterIP —— 而 nginx 在这里是发布过的 ingress class……
和 kubetron 的 authURL、ExternalName 解析器**是同一个形状**,这就是它是守卫而不是备注的原因」。
`auth-url` 是**一模一样的形状,而且一层守卫都没有**。

⚠️⚠️ **诚实说明这一条只验证了一半。** ingress-nginx **不在本环境里**
(`/root` 下有 kubernetes、etcd、external-snapshotter、kyverno、kubetron 和三个 kubezoo 仓,
没有 ingress-nginx),所以关于 `authreq/main.go`、`mirror/main.go`、`alias/main.go`、
`parser/validators.go` 的 `CheckAnnotationRisk` 以及 `annotations-risk-level` 默认值这些
**符号级引用,我没有对着源码核过,不采信**。⇒ 这一条**成立与否取决于**:
(a) 平台的 ingress 控制器是不是 ingress-nginx;
(b) 它的 `annotations-risk-level` / `allow-snippet-annotations` /
`allow-cross-namespace-resources` 怎么配。
⭐ 但**无条件成立的那半**是:**kubezoo 对 Ingress 注解不施加任何策略,控制器认什么租户就写得了什么。**

⚠️ **两处收窄,不改结论:**

- 只有点名**已发布 class** 的 Ingress 才到得了平台控制器 —— `pkg/convert/ingress.go:105-111`
  会把未发布的 class 前缀到租户自己的名字空间里,默认发布集是**空的**
  (`ingress.go:67`,`publishedclass.Static("ingressclass", nil)`)。
  租户自己那个 class 上的注解**不是发现**:那个控制器是租户自己跑的,它自己拨得动。
  ⇒ 这收窄了面,但**没有围住**:点名 `nginx` 不需要任何权限,而且那就是文档写明的
  「租户请求对外暴露」的正路(`ingress.go:44-50`)。
- **`configuration-snippet` 从标题里拿掉。** 它大概率在默认配置下就被
  `allow-snippet-annotations: false` 挡住了,而我在这里核不了。
  ⭐ 但 `auth-url` / `mirror-target` / `server-alias` **都不是 snippet 注解**,不受那个开关管
  —— 去掉它不削弱这一条。

#### 修复(两半,缺一不可)

1. **kubezoo 侧:`refuseIngressAnnotations`,三条写路径全上。**
   ⭐ 这一条**应该是允许清单而不是拒绝**(见 §4),键控在**注解前缀**上:
   `nginx.ingress.kubernetes.io/` 下未在清单里的键一律拒,清单由平台配置,
   与 `pkg/publishedclass` 同一个形状(标签发布、未发布即拒)。
   ⚠️ 别做成「拒掉这三个」—— 那是在跟一个不在本仓的注解表赛跑,必输。
2. **策略层:给 `tenant-ingress-hostnames.yaml` 加第三条 `validations`**,
   覆盖 `metadata.annotations` 里的 `server-alias`。它和已有两条是同一个判据
   (「纯写路径、不需要看响应、且换个平台会变」),放在同一份文件里才不会被下一个人漏掉。
3. **平台侧:把 ingress 控制器的 `annotations-risk-level` 钉下来。**
   ⚠️ 它是**集群级的一个旋钮**,而且总有人为了平台自己的某个 Ingress 把它调高
   —— 所以这一半不能单独用,必须和第 1 条并存。

**最便宜的确认实验:`server-alias`。** 不需要管理网的目标,不需要对风险等级做任何假设:
两个租户,一个用 `server-alias` 认领另一个还没声明的主机名,`spec.rules[].host` 老老实实留在
自己的子域里。VAP 会放行(它只读 spec),而 §AH 那个「先到先得、落败方零报错」的后果,
就从一个 VAP 读不到的字段里复现出来了。

---

### ⛔ ④ kubetron 的 `authURL`:目的地已记录,**时长没有**

目的地那一半是确认实例 #1,也已经记在 `docs/kubetron-knaas-requirements-cn.md:45`(T5)。
本轮新增的是**另一个轴:这次连接会持续多久,以及它占着谁的东西。**

```go
provider, err := openstack.AuthenticatedClient(ctx, gophercloud.AuthOptions{
    IdentityEndpoint: authURL, ...            // pkg/service/client.go:58
})
...
provider.HTTPClient.Timeout = 30 * time.Second // :67 —— 在那次往返之后
```

`pkg/neutron/provider.go:169` / `:178` 是**逐字相同的顺序**。

⭐ **这个仓自己把危险写下来了,然后把封顶加晚了一次调用**
—— `pkg/neutron/provider.go:125-128`:

> Reconciler workers hold a workqueue key for the duration of a Neutron call;
> gophercloud defaults to `http.Client{}` with **NO timeout**, so one hung TCP
> connection would **wedge a worker forever** (§6.6). Cap every call.

**为什么这不只是一次慢调用:**

- reconcile 的 ctx **没有 deadline**:controller-runtime 只在 `ReconciliationTimeout > 0`
  时才包 ctx,kubetron 从不设它;
- Go 的 `DefaultTransport` 只有 30s 的 **DialContext**,**没有 ResponseHeaderTimeout**
  ⇒ 一个**握手成功但不回包**的对端会一直挂着;
- worker 预算是 `MaxConcurrentReconciles: 2`(`pkg/service/reconciler.go:108`,写死)
  加 claim 侧默认 2 —— **一个租户的两个 Service 就能把一个 shard 的 Service reconciler 全部占死**;
- ⛔ **删除路径上挂住(`reconciler.go:483`)会把 namespace 永久卡在 Terminating**:
  `reconcileDelete` 的 `namespaceTerminating` 逃生口只在凭据解析**返回错误**时才走
  (`:455-465`),**挂住不返回任何东西**,finalizer 于是永远不摘。

⚠️ **两处收窄:**

- **爆炸半径是 per-shard,不是全局** —— 拨号在 `myShard` 闸门之后(`reconciler.go:145`),
  只有绑在同一个 shard 上的同伴租户受害。`cmd/manager/main.go:174-181` 的
  cache 只过滤 Pod 和 NetworkPortClaim(Service/EndpointSlice 不过滤)是事实,
  但那花的是内存和事件量,**不是 reconcile 预算**;
- 「租户自己起个监听就够了」这句话**假设了控制面路由得到租户的 Pod IP**,那是部署相关的。
  纯黑洞地址会被 30s 的 DialContext 干净地打掉(只是温和的重试型 DoS)。
  真正的永久挂起需要一个**会 accept 但不回包、且从那个 netns 可达**的对端
  —— 而那个 netns 恰好就是 SSRF 本来就在拨的管理网,所以这完全成立,只是别把前提说漏。

#### 修复

1. **`provider.HTTPClient` 必须在 `AuthenticatedClient` 之前构造好并带 Timeout**
   —— 两处(`client.go:58`、`provider.go:169`)一起改。这是那条注释本来就要求的
   (「**Cap every call**」),现在漏的是第一次。
2. **给 reconcile 设 `ReconciliationTimeout`**,作为第二道 —— 单点封顶总会被下一个新调用绕过。
3. ⚠️ **`lb-cred` 那半不是发现,但 R3 的整改不完整。**
   `reconciler.go:510` 先读 `LBCredAnnotation`,`:511-513` 才被 `CredentialAnnotation` 覆盖,
   而 Secret 从 `svc.Namespace` 读(`:526`)—— **租户自己的 namespace,里面的 `authURL`
   本来就是它写的,没有新增任何能力**。但 `docs/kubetron-knaas-requirements-cn.md:70` 的 R3
   只点名了 `:511` 的 `CredentialAnnotation`,**没点 `:510`**。R3 落地之后,
   `lb-cred` 会变成剩下的唯一那个名字。⇒ **改 R3 一行措辞,把 `:510` 一并划掉**,不是新建一条。
4. ⭐ 真正关掉这个 sink 的是 **R2**(`authURL` 不再来自租户可写来源),与上面两个注解名都无关。

---

## 3. 这轮**没有**确立的东西

⚠️ 不粉饰。下面每一条都是结论依赖但本环境答不了的,以及要答需要什么。

| 没确立的 | 影响哪条 | 要什么才能答 |
|---|---|---|
| **ingress-nginx 是不是平台的控制器,以及它的 `annotations-risk-level` / `allow-snippet-annotations` / `allow-cross-namespace-resources`** | §2③ 的**可利用注解清单**(不是缺口是否存在) | 把 ingress-nginx 拉进来读 `parser/validators.go` 的 `CheckAnnotationRisk` 与 `controller/config/config.go` 的默认值;或者直接在 lab 里试 `server-alias` |
| **上游 apiserver 有没有开 `--enable-aggregator-routing`** | 它会把 webhook 解析从 ClusterIP 切到 EndpointSlice 地址,**把 webhook 的目的地挪到 §2① 的 podIP 锚点上** | 看 apiserver 的静态 Pod manifest,并钉进部署文档 |
| **kata 节点池上有没有 kubelet credential provider 或 `/var/lib/kubelet/config.json`,`matchImages` 的 glob 是什么** | 决定 `spec.containers[].image`(和 `spec.volumes[].image.reference`)是「不是发现」还是 **(b) CREDENTIAL 发现** —— `pkg/kubelet/images/image_manager.go:314` 让租户的镜像串成为**节点自身凭据的选择器** | 看节点配置。一个 `*` 或过宽的通配就直接翻转判定 |
| **平台发布的 StorageClass 有没有在 `csi.storage.k8s.io/*-secret-name` 里用 `${pvc.annotations['...']}`** | kubezoo 不限制 PVC 注解(`pkg/convert/pvc.go:126` 只读 `AnnBoundByController`),那一个字符串决定租户能不能挑 external-provisioner 去取哪个 Secret。⭐ 对照:快照侧**只**支持 content/snapshot 的名字与 namespace 占位符,**根本没有 annotation 占位符**(`/root/external-snapshotter/pkg/utils/util.go:359-399`)—— **provisioner 和 snapshotter 在这里分叉了,只有一边是围住的** | external-provisioner 的源码不在本环境;以及平台实际发布的那几个 StorageClass |
| **有没有平台级 Prometheus 抓租户 Pod,relabel 规则有没有从注解拼 `__address__`(而不是从 `__meta_kubernetes_pod_ip`)** | 如果有,那就是又一个 (b) CREDENTIAL sink(抓取器带 bearer token) | 平台的 Prometheus 配置。这四个仓里唯一的 scrape 注解是 kubetron 给自己打的 |
| **平台的公共 IngressClass 自己带不带 `is-default-class`** | 若带,则 `--public-ingress-classes` 与发布标签**不是** `pkg/convert/ingress.go:49-50` 声称的那个开关(「Empty by default, which leaves every tenant Ingress internal」)—— 省略 `spec.ingressClassName` 就直达公共控制器,那条注释要改 | 看集群里那个 IngressClass 对象 |
| **平台是不是打算让租户一直持有自己的 OpenStack 应用凭据,以及每个租户是不是映射到独立的 project** | §2④ 与 T1/T2/T3 的**多条围堵论证全部压在这两件事上** | 写下来,写在 kubetron 能被对照审计的地方。⚠️ 这两条任一改变,**四个字段同时换类别** |
| **`docs/kubetron-knaas-requirements-cn.md` T3 的事实错误** | T3 把 `NetworkPortClaim.spec.credentialSecretRef` 列成活的租户写入路径,**它不是**:`cmd/kubezoo/app/sharedcrd.go:45` 只共享 `snapshot.storage.k8s.io`,`kubetron.network.kubevirt.io` 对租户不解析 | 改文档。⭐ 这个更正是有分量的:它意味着把那个 group 加进 `sharedCRDResources` 是一次**大的、未经审查的**改动,而不是一行小改 —— `sharedcrd.go:42-44` 自己说了「Making a group resolvable makes every resource in it addressable」 |
| **`docs/DESIGN-ingress-l7.md`(未实现,2026-07-25)** | 它计划:租户 Ingress 上的 `octavia.ingress.kubernetes.io/floatingip`、往**租户可写的** `kubetron-network` ConfigMap 里加 external-network 键、把租户 TLS Secret 传进 Barbican、以及给**带 admin 凭据的** `LBOrphanGC` 删除器引入**第二套名字文法**(`kube_ingress_<cluster>_<ns>_<name>`,5 段,而 `parseLBName` 是 3 或 4 段,`pkg/service/lb_gc.go:140-162`)。⛔ **一个带 admin 凭据的删除器解析两套名字文法,就是误判变成误删的那个形状** | 合并前审计。⚠️ 它的 §5 第 2 条假设 `kubetron-network` ConfigMap 是**运维输入** —— 在 KNaaS 下这是错的 |

⚠️ 还有一条**方法上的**没确立:**有没有合法的东西经 kubezoo 写 pod status?**
kubelet 不经过 kubezoo,所以直觉是没有。唯一要试的反例是**租户自己的 operator 在
reconcile 自己 Pod 的 status**。§2① 的子集规则对这个反例是安全的,但值得在 lab 里确认一次。

---

## 4. ⭐ 哪些是设计决策,不是 bug

**不是每条都该拒。有几条是平台**应该**卖的能力,那就该是一份发布过的允许清单,而不是一条拒绝。**
判据沿用 `refuseInlineCSIVolume` 已经写下的那句(`pkg/proxy/inlinecsi.go:45-53`):
「提供一个是关于某个具体驱动的新决策,**它应该花掉一次对那个驱动的评审**,
就像 `sharedCRDResources` 里的一个条目那样。一份永远为空的允许清单只是把决策搬到没人看的地方。」

| | 决策 | 允许清单键控在什么上 |
|---|---|---|
| **Ingress 注解(§2③)** | ⭐ **应该是允许清单。** 租户要 `auth-url` 做外部认证是完全正当的产品需求,而 `rewrite-target`、`proxy-body-size` 这类根本不拨号的注解**必须**放行 —— 一刀切拒绝会让平台的 ingress 卖不出去 | **注解键前缀 + 目的地**。两级:(1) 键在 `nginx.ingress.kubernetes.io/` 下的**发布过的键名清单**(平台配置,与 `pkg/publishedclass` 同形);(2) 对**值是 URL** 的那几个键(`auth-url`、`mirror-target`),再校验目的地必须落在**该租户自己的 namespace 内**(`http://<svc>.<ns>.svc.cluster.local`,`<ns>` 前缀必须匹配租户 ID)。⭐ 第二级才是关键:它把「拨号」这件事收回租户自己的网络,于是这条轴上的问题消失,而能力保留 |
| **`server-alias`** | ⛔ **不是允许清单,是拒绝** —— 或者说,它归**主机名归属**那套管,不归注解管。它和 `spec.rules[].host` 是同一件事的两种写法,应该受同一条 VAP 约束 | 复用 `tenant-ingress-hostnames.yaml` 的 `tenantSuffix`。⚠️ 那份文件已经写着真实客户要自带域名时需要**每租户白名单 + 域名归属校验**,而且白名单**必须放在租户改不动的地方** —— `server-alias` 直接继承这条,不需要新的产品决策 |
| **`type: LoadBalancer` / kubetron 的 `authURL`(§2④)** | ⭐ **已经是设计决策,而且是对的** —— 租户持自己的 OpenStack 应用凭据、Neutron RBAC 承担访问控制,这是 `claim_controller.go:66-69` 明写的。**问题不在能力在,而在没封顶、没校验 URL** | R2 已经点了:`authURL` 不来自租户可写来源 ⇒ 允许清单键控在**平台侧配置的 Keystone endpoint** 上,租户只提供凭据 ID/Secret。这样能力一分不少,SSRF 归零 |
| **探针 `.host`(§2②)** | ⛔ **纯拒绝,零成本,没有产品讨论的余地。** 空 host 的默认值就是 Pod 自己的 IP;一个租户要探自己的容器,**永远不需要写这个字段**。写了它只有一个意思:探别的地方 | —— |
| **`pods/status` 的 IP(§2①)** | ⛔ **纯拒绝。** kubelet 不经过 kubezoo ⇒ 经 kubezoo 写进来的 pod IP,发起者只可能是租户 | —— |
| **内联 CSI 卷** | ✅ **已经按这个模式做完了** —— `inlinecsi.go:45-53` 明写是「refused rather than allowlisted, and that is the decision rather than a placeholder」。⭐ 本节的判据就是从那里抄来的 | (若将来要开)**驱动名**,一次一评审 |
| **`IngressClass` 的 `is-default-class`** | ⚠️ **不是本轴的发现,但在「谁守这个字段」那条轴上是真的、且几乎零成本**:一个租户建的 IngressClass 带上这个注解就成了**集群级默认**(`plugin/pkg/admission/network/defaultingressclass/admission.go:129-155`,**最新的赢**,而 `111111-` 开头的名字连并列时也赢),此后所有**新建的、没写 class 的** Ingress —— 别的租户的、平台自己的 —— 都被改指到它。⭐ 对照 `proxy.go:1125-1134` 已经为 StorageClass 记下了同一个洞,而两者的差别只是 RBAC:租户对 `storageclasses` 只有读(`clusterscope.go:71-72`),对 `ingressclasses` 有**全套写**(`:137-139`) | 归到下一轮那条轴上处理:在 `pkg/proxy` 里对租户写 `IngressClass` 时拒掉这个注解即可,**无兼容成本**(租户自己的 Ingress 显式点名自己的 class 就行),不像 PVC 空 class 那个案子(`proxy.go:1129-1135`)会伤到存量 |

---

## 5. 一句话总结

这条轴上找到的三条,**没有一条能被前六轮的问法找到**:
`pods/status` 在「谁守它」下面看起来只是个 status 字段;探针 `.host` 看起来是租户探自己的容器;
Ingress 注解看起来是租户调自己的入口。它们全都在**别人的网络里**变成了一次连接。

⭐ 而最该记住的一条是 §2① 的后半:**已经上线的守卫,把一个租户可写的值,拿去校验另一个租户可写的值。**
`refuseForgedEndpointAddress` 的注释准确地指出上游那个检查是「整棵树里唯一一处针对这个字段的检查」
—— 它是,而它的锚点是租户写得动的。**把上游的检查搬到写入端,不会让那个检查变得比它本来更强。**
