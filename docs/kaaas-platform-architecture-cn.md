# KAaaS 平台架构与落地清单

本文记录经多轮评估后确定的目标架构(内部代号 **B1**)、它替代的方案(**B2**)以及被否决的理由,
并把落地前必须完成的工作按依赖顺序列出。

> **进度看 [`../TODO-kaaas.md`](../TODO-kaaas.md)。** 本文回答"为什么这样设计",那份回答
> "还剩什么没做"。**实现中若发现本文写错、或最终采用了别的方案,请先改本文,再回去同步
> TODO 的条目** —— 两份文档不允许长期不一致。

文中**每条事实断言都标注源码位置**,便于代码演进后重新核对。
凡属推演而未实测的,均在该处显式标注 **⚠️ 未验证**。

---

## 1. 目标架构(B1)

```
        租户 PC:kubectl + 平台下发的 kubeconfig
                        │  单一入口,租户身份来自客户端证书 OU
                        ▼
        ┌───────────────────────────────────────┐
        │  kubezoo   控制面多租户               │
        │  namespace/name/group 改写 + 视图隔离  │
        └───────────────┬───────────────────────┘
                        │ impersonate,改写后的对象
                        ▼
        ┌───────────────────────────────────────┐
        │  上游 Kubernetes(平台自有,共享)      │
        │  apiserver / scheduler / controller-mgr│
        │  ├─ Kyverno       准入策略 + generate  │
        │  ├─ kubetron      Neutron/OVN + Octavia│
        │  └─ 存储:KubeBrain                    │
        └───────────────┬───────────────────────┘
                        │
        ┌───────────────▼───────────────────────┐
        │  平台自有 kata 节点池                  │
        │  kubelet → CRI → kata microVM         │
        │  ovs-cni(Multus)→ br-int → OVN 租户网 │
        └───────────────────────────────────────┘
```

### 分层职责

| 层 | 组件 | 负责 |
|---|---|---|
| 控制面多租户 | **kubezoo** | API 视图隔离、租户身份、租户 kubectl 体验 |
| 准入与策略 | **Kyverno** | 安全准入、字段替换、namespace 配套对象生成 |
| 数据面多租户 | **kubetron** | 每租户 OVN 逻辑交换机、Octavia LB、每租户 DNS |
| 计算隔离 | **kata** | per-Pod microVM,不共享内核 |
| 调度与生命周期 | **k8s 原生** | kube-scheduler、kubelet(探针/重启/hook/volume/SA token) |
| 存储 | **KubeBrain** | 上游集群的 etcd |
| 块存储/共享存储 | **CSI**(Cinder / Manila) | PVC |

三个"租户"概念分属不同层,**必须显式对齐**(见 §5):

| | 租户是什么 | 来源 |
|---|---|---|
| kubezoo | 6 位 ID | 客户端证书 `OU`(`cmd/kubezoo/app/server.go:930`) |
| kubetron | OpenStack project | namespace 上的 `kubetron-network` ConfigMap → application credential(`pkg/neutron/provider.go`) |
| Kyverno | 无租户概念 | 靠 namespace 前缀识别 |

---

## 2. 为什么不是 B2(Virtual Kubelet + OpenStack Zun)

B2 曾是首选方案:kubezoo 提供控制面,VK + Zun 提供计算/存储/网络。否决理由如下。

### 2.1 核心论据:VK 场景下没有 kubelet,它的活要全部重写

| 能力 | B1 | B2 |
|---|---|---|
| 探针执行 | ✅ 原生 kubelet | ❌ **VK 框架不执行探针**(`virtual-kubelet@v1.2.1/node/` 下零处引用 `LivenessProbe`/`ReadinessProbe`),provider 自写 |
| restartPolicy / preStop / 优雅终止 | ✅ 原生 | ❌ provider 自写 |
| ConfigMap / Secret / emptyDir 挂载 | ✅ 原生 | ❌ provider 合成 + Zun 侧补丁 |
| **SA token** | ✅ 原生,含自动续期 | ❌ provider 要调 TokenRequest 并**持续续期**(kubelet 在 TTL 剩 20% 时刷新,`kubernetes/pkg/kubelet/token/token_manager.go:190`) |
| PVC | ✅ 原生 CSI | ⚠️ 需对接 |

不实现探针的后果不是"探针无效",而是**租户写的探针语法通过、行为静默失效**:readiness 不生效导致
Service 把流量打给未就绪容器;liveness 不生效导致卡死容器不被重启。

### 2.2 "租户不用买节点"两条路都成立,B2 没有省掉节点

B2 的选择理由是弹性容器、租户零节点成本。但 **B1 的节点同样是平台自有**,租户一分钱不出
(与 Fargate 相同——机队属于云厂商)。两条路平台都要维护一个池:B1 是 k8s kata 节点,
B2 是 Zun compute 节点。

**B2 换来的不是"没有节点",而是"把节点从 k8s 挪进 OpenStack"**,代价是重写 kubelet。

### 2.3 B2 的"无限容量"是虚构的

VK 节点上报的容量是静态配置值(`k8s-zun-provider/config.go` 默认 CPU=20 / Memory=100Gi /
**Pods=20**),与 OpenStack 实际配额无关。真实上限依然存在,只是**调度器看不见** ——
结果是失败点从"调度阶段 Pending"位移到"创建阶段 ContainerCreating 卡死",排障体感更差。

### 2.4 唯一能让 B2 重新占优的场景

**希望容器与 Nova 虚拟机共用 OpenStack 的同一套计算配额、调度与计费**,而不是给 k8s
静态划一块资源池。

注意网络层面 kubetron 已经统一了——容器与 Nova/KubeVirt VM 在同一租户网内平权互通,
所以差别只剩计算配额的归属。若这不是硬需求,B1 成本低一个量级。

### 2.5 Zun 侧调研结论(保留备查)

否决 B2 不是因为 Zun 弱。相反,**当前版本的 Zun 已经是弹性容器服务的骨架**:

| ECI 核心要素 | Zun | 证据 |
|---|---|---|
| Pod 语义(共享 netns) | ✅ 真正的 CRI PodSandbox | `zun/container/cri/driver.py:80` `RunPodSandbox` |
| per-Pod 强隔离沙箱 | ✅ `runtime_handler` 可选 | `cri/driver.py:74,83`(`capsule.runtime or CONF.container_runtime`) |
| VPC 级网络隔离 | ✅ 每 capsule 独立 Neutron port + SG | `_write_cni_metadata` |
| 多租户配额 | ✅ Keystone project 级 | `zun/common/quota.py` |
| 文件注入原语 | ⚠️ 有 `local` 驱动,但**未接到 capsule 路径** | `zun/volume/driver.py:88` vs `api/controllers/v1/capsules.py:425`(硬编码 `volume_driver = "cinder"`) |
| logs / exec / attach / stats | ✅ API 齐全 | `api/controllers/v1/containers.py:938/973/1043/1175` |

短板全在 provider(`/root/k8s-zun-provider`),它是原型:单一 OpenStack project
(`zun.go:47` `AuthOptionsFromEnv`)、`Spec.Volumes` 从未赋值(唯一赋值是 `zun.go:156` 的
`Spec.Containers`)、`RunInContainer` 是 no-op(`zun.go:218`)、带 resource limits 必 panic
(`zun.go:236` 向 nil map 赋值,已用等价程序验证)。

**若将来 §2.4 的场景成立,这份清单可直接作为 B2 的工作量基线。**

---

## 3. 租户体验与能力边界

### 3.1 租户拿到什么

**不是 `/etc/kubernetes/admin.conf`。** 那是上游集群的管理员凭据,发给租户等同交出整个集群。

平台下发的是 kubezoo 为该租户**单独签发**的 kubeconfig,从 Tenant 对象注解取出:

```shell
kubectl get tenant 111111 \
  -o jsonpath='{.metadata.annotations.kubezoo\.io/tenant\.kubeconfig\.base64}' \
  | base64 -d > 111111.kubeconfig
```

证书内容(`pkg/util/certs.go:99-100`):

```
OU = 111111            (6 位租户 ID)
CN = 111111-admin
```

认证时三个条件必须同时成立才认定租户身份(`cmd/kubezoo/app/server.go:930`):
`len(OU[0]) == 6`、`OU[0] == CN[:6]`、`CN[6] == '-'`。

### 3.2 租户能发什么

✅ Pod / Deployment / StatefulSet / Job / ConfigMap / Secret / Service / RBAC / 自己的 CRD

❌ 或无意义:

| 资源/字段 | 原因 |
|---|---|
| DaemonSet | 语义可用(B1 有真实节点)但租户不应感知节点;应由策略拒绝 |
| Node | 应不可见(现状见 §7.1) |
| hostNetwork / hostPath / privileged / hostPID / hostIPC | 由 Kyverno + PSA 拒绝 |
| **平台安装的任何 CRD**(kubetron / Kyverno / OpenKruise 等) | CRD 按名字前缀过滤(`pkg/util/util.go:311`),且 **FAQ 描述的 system CRD 共享机制未实现**(见 §7.4) |

---

## 4. 落地链路:租户发布一个 Pod

```
租户 kubectl apply
  → kubezoo:namespace default → 111111-default,impersonate 为 111111-admin
  → 上游 apiserver 准入链:
        Kyverno   注入 runtimeClassName、清 tolerations、拒 nodeName…
        kubetron  webhook 变异(Multus 注解、探针改写、readinessGate)
  → 存进 KubeBrain
  → 上游 kube-scheduler 分配 nodeName
  → 平台 kata 节点的 kubelet 经 CRI 拉起 microVM
  → ovs-cni(Multus)把 eth0 插进 br-int,ovn-controller 绑定逻辑端口
  → kubetron binding controller 写回 binding:host_id,端口 ACTIVE 后翻转就绪门
  → Pod Ready == 网络真通
```

调度由**上游那个共享的标准 kube-scheduler** 完成,租户看不见也配不了。
探针、restartPolicy、优雅终止全部由原生 kubelet 执行——这正是 B1 相对 B2 白拿的部分。

---

## 5. 三方对象契约(必须先定)

kubezoo、kubetron、Kyverno **都会往租户的对象上写东西**,而只有 kubezoo 在出站口能擦。
契约定得越晚,kubezoo 的 convert 层越会变成一堆特例补丁——而那正是最不该复杂化的地方(§6)。

### 5.1 各方写入的内容

| 来源 | 写入 | 位置 |
|---|---|---|
| kubetron | `k8s.v1.cni.cncf.io/networks`、default-network 注解 | `pkg/webhook/mutate.go:75,179` |
| kubetron | `SecondaryClaims` / `IdentityPending` 注解、`ShardLabel` | `mutate.go:183,77,89,192` |
| kubetron | **探针 `httpGet`/`tcpSocket` → `exec`** | `pkg/webhook/probes.go:65-66,77-78` |
| Kyverno | 注入的 `runtimeClassName`、清空的 `tolerations`、改写的 `securityContext` | 策略决定 |
| 调度器 | `spec.nodeName`(暴露平台节点名) | — |

### 5.2 后果

租户 `kubectl get pod -o yaml` 会看到这些。尤其**他写的 `httpGet` 探针变成了 `exec`**。
影响:

- GitOps(ArgoCD/Flux)持续报漂移,`kubectl diff` 永不干净
- 租户排障时对不上("我明明写的 httpGet")
- `nodeName` 泄露平台节点名,可推断哪些租户共处一台机器

### 5.3 契约要求

1. **约定一份"平台内部字段"清单**,kubezoo 在出站时统一擦除。
2. **被改写的字段必须可还原** —— kubetron 需把原探针存进约定注解,kubezoo 出站时还原。
   这是跨项目接口,**必须先定再实现**,不能事后补。
3. 每新增一个会变异租户对象的平台组件,必须同步登记进这份清单。

---

## 6. 安全模型

### 6.1 基本事实:改写层是当前唯一防线

每个租户在上游被授予的是全权(`pkg/controller/controller.go:556`,绑定 `:614`):

```go
// a "root" role which can do absolutely anything
Name:  tenantId + "-cluster-admin"
Rules: NewRule("*").Groups("*").Resources("*")
```

加上示例部署中 kubezoo 自身 `--authorization-mode=AlwaysAllow`,结论是:

> **租户隔离完全且唯一地依赖 `pkg/convert/` 改写层的正确性,没有任何兜底。**

改写规则本身只有一条(`pkg/util/util.go:146`):namespace 级看 namespace 前缀,
cluster 级看 name 前缀,CRD 看 group 前缀。

### 6.2 ⭐ 第一优先改动:per-namespace RBAC(建立第二道防线)

**租户创建 namespace 时,同步在该 namespace 内生成 RoleBinding**,把权限限死在自己的
namespace。这样即使 convert 层漏掉一个引用字段,上游 RBAC 也会拒绝。

全部在 kubezoo 的 tenant controller 内完成,不需要改 Kubernetes。

⚠️ **硬边界**:RBAC 的 `resourceNames` 是**精确匹配**,不支持前缀或通配
(`kubernetes/pkg/apis/rbac/v1/evaluation_helpers.go:86`:`ruleName == requestedName`)。

| 资源类别 | RBAC 能否兜底 |
|---|---|
| **Namespaced**(Pod / Deployment / ConfigMap / Secret… 40+ 种) | ✅ per-namespace RoleBinding |
| **Cluster-scoped**(PV / StorageClass / Namespace / CRD… 20+ 种) | ❌ 名字动态生成,RBAC 事先列不出来 |

Cluster-scoped 那部分**永远只能靠改写层**。但绝大多数流量是 namespaced,收益很大。

**这一条应排在所有 kubezoo 改动的最前面**,因为它决定后续隔离审计的严苛程度:
从"必须做到零遗漏,否则即越权"降级为"找漏,漏了还有网"。

它顺带解决另一个问题:**限制租户对自己 namespace 的 update 权限**,防止摘除策略标签(§7.5)。

### 6.3 安全项三分类

**① 能直接堵**

| 项 | 改法 | 代价 |
|---|---|---|
| Node 无条件可见(§7.1) | 删掉 TODO 分支 | Conformance 测试会挂——它正是为此而加 |
| 平台指纹泄露(§5) | convert 层出站擦除/还原 | 需三方契约 |
| `-A` 全量 LIST(§7.2) | 先取租户 namespace 列表,再逐 namespace 发 scoped LIST 合并 | 请求放大(N 个 ns = N 次上游请求),但数据量从全集群降到租户自身 |

**② 纵深防御** —— §6.2 的 per-namespace RBAC。

**③ 只能缓解,改不掉**

- **共享 apiserver + 共享 etcd 的爆炸半径**:一个租户触发慢查询/OOM/bug,全体受影响。
  缓解手段是限流(见 §8)。
- **共享节点的侧信道**:同一台 kata 节点承载多租户。kata 挡住内核共享,但 CPU 缓存侧信道、
  噪声邻居是物理层面的。
- **cluster-scoped 资源无 RBAC 兜底**(§6.2)。

### 6.4 结论

能堵到"**共享集群 + 纵深防御**"的水平,堵不到"每租户独立集群"的水平。
面向外部客户时,这个差别会被合规审计问到,应提前准备说明。

---

## 7. 已知缺陷与不一致(kubezoo)

### 7.1 Node 对所有租户无条件可见

`pkg/util/util.go:136-144`:

```go
// Todo: renjs, temporarily expose nodes for tenants to pass Conformance test
if t.GetAPIVersion() == "v1" && t.GetKind() == "Node" {
    return true
}
```

租户可列出整个共享集群的全部节点(名称、标签、容量、地址、镜像列表)。
在 B1 下更敏感——节点上跑着所有租户的负载,叠加 Pod `nodeName` 可见,
租户能拼出"哪些租户共处一台机器"。

### 7.2 `-A` 与 cluster-scoped 请求走"全量 + 过滤"

`pkg/proxy/proxy.go:178-180` 的 `getClient()` 只在 `requestInfo.Namespace != ""` 时限定
namespace;否则客户端不限范围,靠 `FilterUnstructuredList`(`pkg/util/util.go:408`)事后筛。

| 租户操作 | 上游查询范围 | 隔离靠什么 |
|---|---|---|
| `kubectl get pods`(带 ns) | 限定 `111111-default` | ✅ 查询范围够不着别人 |
| `kubectl get pods -A` | **全集群** | ⚠️ 拉回全部再内存过滤 |
| cluster 级资源 | **全集群** | ⚠️ 同上 |
| watch | 同上两种 | `pkg/proxy/watch.go:91` 逐事件过滤 |

两个后果:

- **安全**:过滤函数 `UpstreamObjectBelongsToTenant`(`util.go:120`)是唯一防线。
  §7.1 的 Node 泄露走的正是这条路。
- **规模**:`pkg/proxy/` 内**零处 informer/cache**,每次真打上游。任一租户敲一次
  `kubectl get pods -A` 就是一次全集群 LIST。租户越多单次越贵,**代价由全体承担**,
  同时构成 DoS 面。

### 7.3 DaemonSet 未在代理层拒绝

FAQ 称限制 daemonset,但 `cmd/kubezoo/app/apigroups.go:557-573` 正常注册并代理,
无拒绝逻辑。需由 Kyverno 策略补上。

### 7.4 system CRD 共享机制未实现

FAQ 描述"system CRD 可配置策略供一个或多个租户使用",但**全仓零命中**。
`ListCRDsForTenant`(`pkg/util/util.go:311`)仅按名字前缀过滤。

后果:**租户看不到也用不了任何平台安装的 CRD**。若要让租户使用 OpenKruise
的 CloneSet 之类,须先实现共享机制。

(反向也成立:Kyverno 的 ConstraintTemplate/Policy、kubetron 的 NetworkPortClaim
对租户天然不可见——策略内容不泄露。)

### 7.5 租户持有自己 namespace 的写权限

namespace 由租户创建、租户拥有 `*` on `*`,因此租户可以编辑它——包括摘掉平台打上的
PSA 标签或策略匹配标签。这直接引出 §8.1 的铁律。

### 7.6 版本停在 1.24

`go.mod` 锁 `k8s.io/kubernetes v1.24.0` / `go 1.18`;README 明示上限 1.24。
kubetron 使用 `k8s v0.36` / gophercloud v2。**两者要在同一集群共存,kubezoo 必须先抬版本**。
这是 B1 的头号前置工作(§10)。

---

## 8. 准入策略层(Kyverno)

### 8.1 ⭐ 铁律:策略匹配一律反向写

Kyverno / Gatekeeper 都按 namespace 或 namespaceSelector 选目标。**禁止正向匹配租户
namespace 的标签**——因为租户能编辑自己的 namespace(§7.5),摘掉标签即可**绕过整套策略**,
而此时他仍有全权建 Pod,于是 runtimeClassName 不注入、nodeName 不拦、tolerations 不清,
**B1 的隔离前提全部落空**。

正确写法是**匹配全部、排除平台自身 namespace**:

```yaml
exclude:
  any:
  - resources:
      namespaces: [kube-system, kyverno, kubetron-system, ...]
```

配合 §6.2 的 per-namespace RBAC 限制租户对 namespace 的 update,双保险。

> **同族问题已在仓库内出现一次**:配额 webhook 的 `objectSelector` 按对象标签排除,
> 而标签由租户掌控 ⇒ 打个标签即可绕过配额(§9.3)。
> **通用规则:排除条件只能建立在租户无法控制的东西上**(平台 namespace、平台签发的凭据),
> 绝不能是租户对象上的 label/annotation。

### 8.2 必备策略清单

| 策略 | 优先级 | 说明 |
|---|---|---|
| **强制注入 `runtimeClassName=<kata>`** | **P0** | B1 的隔离前提。租户不写默认是 runc,与其他租户共享内核;且 RuntimeClass 是 cluster-scoped(`util.go:275`),租户写 `kata` 会被改写成 `111111-kata` 而不存在,所以**只能由平台强制注入** |
| **拒绝 `spec.nodeName`** | **P0** | 直接绕过调度器,把 Pod 钉到任意节点 |
| **清空/白名单 `tolerations`** | **P0** | 否则可跑到不该跑的节点,包括控制面节点 |
| PSA `restricted` 等价规则 | **P0** | hostNetwork / hostPID / hostIPC / privileged / hostPath |
| 限制 `nodeSelector` / `affinity` 到允许标签 | P1 | 节点标签对租户可见(§7.1) |
| 强制 `schedulerName` | P1 | |
| 拒绝 DaemonSet | P1 | 补 §7.3 |

前四条 PSA 管不了(PSA 不覆盖 nodeName/tolerations/nodeSelector),必须由策略引擎实现。

### 8.3 为什么选 Kyverno 而非 Gatekeeper

| 理由 | 说明 |
|---|---|
| ⭐ **`generate` 规则省掉一个控制器** | 见 §8.4。Gatekeeper 只有 validate + mutate |
| **mutation 更强更易写** | `mutate.patchStrategicMerge` 几行 YAML;Gatekeeper 需 `Assign`/`AssignMetadata` 独立 CRD |
| **YAML 而非 Rego** | 策略要被多人维护与合规审阅,Rego 是实打实的学习成本 |

Gatekeeper 的优势(供将来重新评估):Rego 表达力更强、audit 机制更成熟、超大规模运行经验更多。
若策略以复杂逻辑判断为主且无 generate 需求,Gatekeeper 更稳。

### 8.4 用 `generate` 承接 namespace 配套对象

租户创建 namespace 时需自动配套的东西,全部交给 Kyverno `generate` + `synchronize: true`
(租户删除后自动重建):

| 生成物 | 用途 |
|---|---|
| per-namespace RoleBinding | §6.2 纵深防御 |
| `kubetron-network` ConfigMap | 绑定该租户的 OpenStack application credential |
| PSA 标签 | 基线加固 |
| ResourceQuota / LimitRange | 配额(与 §9 的 ClusterResourceQuota 配合) |

这一条直接抵掉原本需要自研的"租户 namespace 控制器"。

### 8.5 `failurePolicy` 的取舍

- `Fail` → Kyverno 挂了,**全体租户建不了 Pod**
- `Ignore` → 策略**静默失效**,回到 §8.1 的绕过场景

**建议 `Fail` + 多副本 + PDB**。理由:`Ignore` 的失效是静默的、且直接击穿隔离前提。

---

## 9. 配额:ClusterResourceQuota

kubezoo 自带 `ClusterResourceQuota`(CRQ,`pkg/apis/quota/v1alpha1/clusterresourcequota_types.go`),
cluster 级资源,spec 内嵌标准 `corev1.ResourceQuotaSpec`,附加 `namespaceSelector` / `namespaces`;
带独立 webhook 与二进制(`cmd/clusterresourcequota`)。
`Tenant` 对象上另有 `Hard corev1.ResourceList`(`pkg/apis/tenant/v1alpha1/types.go:72`)。

### 9.1 实现机制:投影 + 聚合 + 双层强制

```
ClusterResourceQuota
   │
   ├─① 控制器在每个匹配的 namespace 投影一个普通 ResourceQuota
   │     Spec = clusterquota.Spec.ResourceQuotaSpec       ← 逐字复制,限额与 CRQ 相同
   │     带 ownerRef → CRQ,带 auto-update 标签
   │     (clusterresourcequota_controller.go: ensureResourceQuotaInNamespace)
   │
   ├─② 上游 kube-controller-manager 负责算这些 ResourceQuota 的 .status.used
   │
   ├─③ CRQ 控制器把各 namespace 的 used 相加写回 CRQ.status
   │     (clusterresourcequota_controller.go:145-147 syncStatus)
   │
   └─④ kubezoo webhook 拦 Pod CREATE:取 namespace 内带标签的 ResourceQuota,
        但把 .status 替换成 CRQ 的聚合用量,再交标准 evaluator 判断
        (controllers/webhook.go:130-162 GetQuotas)
```

评估器复用 Kubernetes 官方实现(`k8s.io/apiserver/pkg/admission/plugin/resourcequota`
+ `k8s.io/kubernetes/pkg/quota/v1/install`),因此**维度定义是标准那一套**。

关键在于**有两层在同时强制,覆盖面不同**:

| 强制者 | 覆盖维度 | 限额语义 |
|---|---|---|
| 上游原生 ResourceQuota 准入插件(默认启用) | **全维度** | **per-namespace** |
| kubezoo CRQ webhook | **仅 Pod CREATE** | **租户总量** |

webhook 的注册规则限死了第二层的范围(`config/setup/quota.tmpl.yaml`):

```yaml
rules:
- apiGroups: [""]
  apiVersions: [v1]
  operations: [CREATE]
  resources: [pods]
```

### 9.2 实际生效范围

| 维度 | 租户总量 | per-namespace |
|---|---|---|
| **compute**:`cpu` / `memory` / `requests.*` / `limits.*` / `*.ephemeral-storage` / 扩展资源(GPU) | ✅ | ✅ |
| **`pods`** 计数 | ✅ | ✅ |
| scopes(`BestEffort` / `Terminating` / `PriorityClass` / `CrossNamespacePodAffinity`) | ✅ | ✅ |
| **对象计数**:`configmaps` / `secrets` / `services` / `persistentvolumeclaims` / `count/deployments.apps` / `services.loadbalancers` / `services.nodeports` … | ❌ **无总量** | ✅ |
| **storage**:`requests.storage` / `<sc>.storageclass.storage.k8s.io/*` | ❌ **无总量** | ✅ |

> **只有 compute 类真正受"租户总量"约束。**
> 对象计数与 storage 类仅有 per-namespace 限额,而该限额值**等于**总量额度
> (投影是逐字复制)⇒ **租户每多建一个 namespace 就多拿一份额度**,而租户可自由建 namespace。

### 9.3 ⭐ 可利用的绕过:objectSelector 用了租户可控的标签

`config/setup/quota.tmpl.yaml` 的 webhook 配置:

```yaml
objectSelector:
  matchExpressions:
  - key: app
    operator: NotIn
    values: [kubezoo-cluster-resource-quota]
```

用意是防自举死锁(配额组件自身的 Pod 不被自己拦)。但 **`objectSelector` 匹配的是对象自身的
标签,而租户完全掌控自己 Pod 的标签**——kubezoo 的改写层不触碰 labels。

**租户只需给 Pod 打上 `app: kubezoo-cluster-resource-quota`,webhook 即不触发,租户总量约束消失**,
只剩 per-namespace 那一层;再配合多建 namespace,compute 额度同样可无限扩张。

这与 §8.1 的铁律是**同一族问题:排除条件必须建立在租户无法控制的东西上**。
正确做法是按 **namespace** 排除(平台自身 namespace),而非按对象标签。

### 9.4 另外两个问题

**并发超发。** `UpdateQuotaStatus` 是空实现(`controllers/webhook.go:163-165` 直接 `return nil`)。
标准 evaluator 依赖该回写完成 admission 期的乐观并发记账;禁用后用量只能等控制器下一轮聚合才收敛
⇒ **突发创建会超出总额**,幅度取决于聚合延迟。

**单点。** `quota.tmpl.yaml` 里 `replicas: 1` + `failurePolicy: Fail` + `timeoutSeconds: 5`。
方向正确(`Fail` 优于静默失效,理由同 §8.5),但单副本意味着**配额组件挂掉时全体租户建不了 Pod**。

### 9.5 落地待办

| 项 | 优先级 | 说明 |
|---|---|---|
| `objectSelector` 改为按 namespace 排除 | **P0** | §9.3,可直接利用的绕过 |
| webhook 规则扩到 configmaps / secrets / services / pvc / CRD 的 CREATE;或改为限制租户可建的 namespace 数量 | **P0** | 否则 §9.2 中对象计数与 storage 类形同虚设 |
| 实现 `UpdateQuotaStatus`,或接受超发并压测出实际幅度 | P1 | §9.4 |
| 配额组件多副本 + PDB | P1 | §9.4 |

⚠️ **上述均为源码阅读结论,未做运行时验证** —— §9.2 的生效范围、§9.3 的绕过、§9.4 的超发幅度
都应在隔离审计(任务 #82)中用真实客户端复现确认,每条带负向对照。

---

## 9.5 租户停机:两种模式 —— 需求已定,未实现

两个来自不同场景的需求,共用同一套机制但**收敛程度不同**:

| 模式 | 场景 | 租户能力 | 负载 |
|---|---|---|---|
| **A read-only** | 欠费保号 | 能看,不能改 | 照跑 |
| **B 完全吊销** | 违法调查 / 取证冻结 | **什么都不能做** | **原样冻结,等待取证** |

A 的用意是催缴而不制造事故 —— 全断会让租户看不到自己的东西,容易误判成"数据没了"。
B 的用意相反:租户**不得再操作**,而 Pod 必须保持原状供取证。

### 机制可行,已实测

#87 之后租户在上游的权限**完全**来自 kubezoo 建的 RBAC(每 namespace 一个 RoleBinding +
一个 ClusterRoleBinding)。删掉它们即实测:

| | 结果 |
|---|---|
| 租户 `kubectl get pod` | **Forbidden** |
| 租户的 Pod | **Running,照跑不误** |

正是需要的语义 —— 负载由 kubelet 与各控制器以**各自的身份**维持,与租户凭据无关。

### 两种模式的实现差异

- **A read-only**:把 RoleBinding / ClusterRoleBinding 的 `roleRef` 指向只读角色
  (`get`/`list`/`watch`),而不是删除绑定
- **B 完全吊销**:删除绑定

⚠️ **`roleRef` 在 k8s 里是不可变字段**,所以 A 的切换必须"删了重建",不能 update。

### ⚠️⚠️ B(取证冻结)有一个容易致命的盲区

**吊销租户的 kubeconfig 并不能阻止租户已部署的东西继续动。**

租户可以给自己 namespace 里的 ServiceAccount 授权(`pkg/convert/rolebinding.go` 的
`transformSubjectToUpstream` 明确支持 `ServiceAccount` 与 `system:serviceaccounts:` 组主体),
这些权限**属于 SA 而不是租户凭据**。租户跑的 operator/控制器拿的是 SA token,
吊销 kubeconfig 对它毫无影响 —— 它可以继续改动、甚至**删除证据**。

所以 B 必须同时处理:

1. 删掉 kubezoo 建的租户 RBAC(收回人的操作能力)
2. ⭐ **一并中和租户自建的 RoleBinding / ClusterRoleBinding**,否则其 SA 仍有权限
3. 考虑是否要冻结 Pod 本身(如打 taint / 阻止重建),取决于取证要求 —— 注意
   **删除 Deployment 的控制器权限一旦还在,证据就可能被自动化流程清掉**

> A 不需要第 2 条:欠费场景下租户的应用**本来就该继续正常运行**,其 SA 权限属于应用的一部分。
> **两种模式的差别正在这里,不能用同一套动作实现。**

### 落地要点

- **状态是一等公民**:必须是 Tenant CR 上的字段(如 `spec.suspended`),
  ⚠️ **不能靠人工删 RBAC** —— 控制器会收敛,手工删的会被建回来(#87 之后如此)
- **两层执行,互为兜底**:
  1. kubezoo 前门按租户状态**拒绝写操作并给出明确文案**(如"租户已停机,续费后恢复管理"),
     否则租户只会拿到一个上游透出的裸 `Forbidden`,不知所云
  2. 上游 RBAC 同时降级为只读 —— 即便前门被绕过也拦得住
- **宽限期到期**才走退租(退租的强制 finalizer 清理已实现)
- **B 需要审计留痕**:何时冻结、谁授权、冻结时的对象清单快照 —— 取证场景下这本身就是证据链的一部分

> 关联任务见 `TODO-kaaas.md`。

## 10. 已知的规模墙

按预计撞上的先后排列。

| # | 墙 | 触发条件 | 关联 |
|---|---|---|---|
| 1 | **`-A` 全量 LIST 无 cache** | 任一租户执行 `kubectl get pods -A`;租户数越多单次越贵 | §7.2;打在 KubeBrain 上即 #41/#43 类问题 |
| 2 | **准入 webhook 同步开销** | 高频短任务创建 Pod,每次同步过 Kyverno | §8.5 |
| 3 | **策略引擎的集群状态缓存** | Kyverno `context` lookup / Gatekeeper referential constraint 需 cache 全集群对象 → 全量 watch | 与 #1 同类。**能不用就不用** |
| 4 | **上游 etcd 单一键空间** | N 租户全部对象共用一套 keyspace | 任务 #84;这是 KubeBrain 的主场,也是产品天花板 |

第 1、2 类"单租户拖垮全体"的问题,缓解手段是在 kubezoo 前面叠 **kubegateway 的每租户限流**
(已在任务 #81 中完成现场验证:每租户限流/熔断/降级、双网关 HA、全局限流跨副本汇总)。

---

## 10.5 `-A` 全量 LIST:现状、过渡方案、以及结合 KubeBrain 的目标解

### 现状(实测)

租户敲 `kubectl get pods -A` 时,kubezoo 走的是**全集群 LIST → 内存里按租户前缀丢弃**
(`tenantProxy.getClient` 在无 namespace 时用集群级客户端,再由 `FilterUnstructuredList` 过滤)。
代价与**全体租户的数据量**成正比 —— 100 租户 × 1000 Pod,任何一个租户随手一敲就是 10 万对象的读取。
既是规模墙,也是**任何租户都能发的 DoS**。

⚠️ 顺带说明:#87 的 per-namespace RBAC 落地后,`-A` 对租户**已经直接 Forbidden**,
所以这条路径当前打不通。但它只是被权限挡住了,**问题本身没解决**,而且 `-A` 这个常用能力也没了。

### 过渡方案:逐 namespace scoped LIST 合并

解决**唯一不可接受**的那条:跨租户放大与 DoS 消失,代价降到与该租户自身数据成正比。

但要带着三个明确决定去做,否则是换个地方踩:

1. **请求放大** —— 一个租户请求变 N 个上游请求(N = 该租户 namespace 数)。
   ⇒ **租户 namespace 数必须有配额约束**,否则请求放大本身成为新的 DoS
2. **分页** —— ⚠️ 现有实现**根本没有 continue token 处理**(全量读回再过滤)。
   合并之后 N 个游标要合成一个连贯 token,否则 kubezoo 必须把租户全部数据 materialize 在内存里。
   ⇒ 要么实现复合 token,要么**明确声明 `-A` 不支持分页**并对结果规模设硬上限(超限报错,不是 OOM)
3. **resourceVersion** —— 合并列表没有单一 RV,N 次 LIST 取自不同 revision,不是快照。
   而 `kubectl get -w -A` 与 informer 都要拿 LIST 的 RV 去起 watch。
   ⇒ 返回 N 个结果中的**最小 RV**(宁可重放也不漏),并写明 `-A` 的 watch 语义是最终一致而非快照

### ⭐ 目标解:结合 KubeBrain 做到 O(租户数据)

**因为 kubezoo 给 namespace 加了租户前缀,一个租户某类资源的全部对象在存储层本来就是
一段连续的 key 区间** —— apiserver 的 `NamespaceKeyRootFunc` 就是
`prefix + "/" + namespace` 拼前缀,所以 `/registry/pods/<tid>-` 是一个天然的连续区间。

一次前缀扫描即可:**O(租户数据)、单一 RV、分页天然正确** —— 上面三个问题一次全消。

⚠️ **但缺口不在存储层,而在 apiserver。** KubeBrain 侧的能力是现成的(前缀区间扫描、RangeStream),
真正缺的是**没有办法把"按 namespace 前缀 LIST"这个意图从 API 表达下去**:
k8s 的 LIST 只有"全部 namespace"或"精确某个 namespace",field selector 对 `metadata.namespace`
也只能精确匹配。kubezoo 打的是真 apiserver,绕不过去。

因此目标解需要一个 **apiserver 侧的改动**(我们本来就维护 k8s fork):
让 LIST 能表达 namespace 前缀,由 store 层把它算成 `/registry/<res>/<tid>-` 这一段区间下发。
这与 store 现有的行为是同构的 —— **它本来就是在算前缀区间,只是现在只会用完整 namespace 去算**。

### 取舍:为什么不让 kubezoo 直连 KubeBrain

"KubeBrain 是我们自己的,直接读多快" —— 这个方案被认真评估过,**结论是不走,但理由不是想当然的那几条**。
下面把两边都记下来,免得以后被当成没考虑过而重新提议。

**先纠正两条常被拿来反对、但其实站不住的理由**:

- ❌ "要复刻 apiserver 的编解码" —— **不成立**。kubezoo 已经在构造完整的
  `kubeapiserver.NewStorageFactoryConfig().Complete(...).New()`,那个 StorageFactory
  本来就掌握每个资源的 codec、存储版本与媒体类型。再建一个指向上游 KubeBrain 的实例,解码基本白送
- ❌ "绕过鉴权就不安全" —— 这条**反而对直连有利**。前缀 `/registry/<res>/<tid>-` 本身就是边界,
  而且是 **RBAC 表达不了的那种**(`resourceNames` 精确匹配,见 §9 的硬边界)。区间读比 RBAC 更严

**真正决定不走的四条**:

1. ⭐ **边界从外部检查变成内部计算**。今天每个租户请求上游都会被 RBAC **独立**校验一次
   impersonate 身份;直连之后,租户边界完全等于"kubezoo 自己算对了那个前缀",**背后没有任何兜底**。
   这不是理论担忧 —— 隔离审计(#82)在 kubezoo 自己的边界逻辑里找到了**三个错**
   (PV 走 nopeConvertor 完全不改写、webhook 全集群生效、`escalate`/`bind` 提权),
   其中两个存在已久且单元测试全过。**刚发现完这些,就把批量读的唯一边界交给同一套逻辑,方向是反的**
2. **静态加密的密钥**。多租户平台存着租户 secret,`--encryption-provider-config` 该开。
   开了之后 kubezoo 必须持有同一套 DEK/KEK ⇒ **它就有能力解密那个 etcd 里的一切**,
   而不只是它该读的那段区间。区间限制是 kubezoo 自己的代码在守,不是密钥在守
3. **审计断了**。直连读不进 apiserver 审计日志,"谁读了什么"会消失 ——
   与 §9.5 的取证冻结需求直接冲突。要补就得 kubezoo 自建一套
4. **它是混合路径不是替代**。聚合 API(metrics 等)根本不在 etcd 里,仍须走 apiserver ⇒
   两条路并存,长期维护两份语义

**而 apiserver 改法反而更省**:改 store 让 LIST 能表达 namespace 前缀,是让它**少算一截**
(它今天就在算前缀区间);直连则要第二套存储栈 + 密钥材料 + 自建审计 + 自己实现 selector 求值
+ 长期绑定 key 布局。**工作量更小,而且 RBAC 与审计都还在。**

**何时该重新评估**:如果部署形态确定不开静态加密、且读审计由别处承担(如网关层),
那么第 2、3 条消失,只剩第 1 条 —— 那时可以把直连作为**只读旁路**重新讨论,
但前提是 kubezoo 的前缀计算有独立的守卫测试与现场负向对照。

> 关联:#84(kubezoo × KubeBrain 键空间形态)、TODO 1.2。

## 11. 前置工作与建议顺序

| 序 | 工作 | 理由 |
|---|---|---|
| **1** | **kubezoo 版本移植 1.24 → 当前**(任务 #83) | kubezoo 与 kubetron 必须共存于同一集群,版本不通则一切免谈。先验 `hack/` 下 codegen 能否运行 |
| **2** | **per-namespace RBAC**(§6.2) | 决定隔离审计的严苛程度,应先于审计 |
| **3** | **三方对象契约**(§5) | 越晚定,convert 层越会碎片化 |
| **4** | 三处粘合层 | ① DNS zone 用租户视角名(§12.1);② namespace 配套生成(交给 Kyverno generate,§8.4);③ kubetron webhook 按 namespace 前缀识别租户 |
| **5** | **隔离正确性审计**(任务 #82) | 转换器静态覆盖 + 双租户真实客户端黑盒穿越测试 |
| **6** | 准入策略实现(§8.2) | |
| **7** | 规模验证(任务 #84) | |

---

## 12. 粘合层细节

### 12.1 DNS:zone 用租户视角的 namespace 名

kubetron 的 DNS zone 由它自己按 namespace 渲染,并由**该租户网内的 CoreDNS Pod** 提供解析
(`pkg/controller/dns_controller.go:143`):

```go
owner := fmt.Sprintf("%s.%s.svc.%s.", svc.Name, ns, clusterDomain)
```

在 kubezoo 下 `ns` 是 `111111-default`,而租户查询的是 `my-svc.default.svc.cluster.local`。

**解法:渲染时使用租户视角的 namespace 名**(剥去 `<tid>-` 前缀)。

这**不是 CoreDNS rewrite,而是一开始就写对名字** —— 无需按源 IP 判别、无需正则、
无反向解析问题。之所以可行,是因为 kubetron 本来就是**每租户独立 zone + 独立 CoreDNS**,
而非共享 CoreDNS。

### 12.2 租户身份对齐

建立 `kubezoo tenantID(6 位) ↔ OpenStack project / application credential` 的一一映射,
在租户创建 namespace 时由 Kyverno `generate` 落下 `kubetron-network` ConfigMap
(§8.4)。kubetron 的 webhook 在上游看到的 namespace 是 `111111-...`,前 6 位即租户 ID,
与 kubezoo 的模型天然对齐。

### 12.3 拒绝消息的泄露面

Kyverno 拒绝时 message 通常包含策略名、namespace 全名、白名单内容。
kubezoo 有 `TrimTenantIDFromError`(`pkg/proxy/proxy.go` 内 9 处调用,如 `:516`)可擦租户前缀,
但擦不掉策略名与平台标签白名单。**⚠️ 未验证** —— 需实测租户看到的拒绝消息内容。

---

## 13. 暂不引入

**OpenKruise。** 两个原因:

1. **租户用不了** —— 其 CRD 受 §7.4 的限制,租户不可见。
2. **平台侧唯一有价值的是 `ImagePullJob` 镜像预热,但收益未知。**
   它与 kubetron 的端口预热池正交(后者解决网络准备,已通过 Knative 实测:
   暖请求 39ms、scale-to-zero 首请求 5.3s;镜像拉取不在这个数字里),
   但 ImagePullJob 只能预拉**已知**镜像——对租户任意上传的私有镜像无效。
   **收益完全取决于镜像复用率,在拿到真实数据前不应据此选型。**

需澄清一处早期误述:kubezoo 所说的"秒级"指的是**租户交付**
("second-level massive tenant lifecycle management",`docs/design.md`),
"短任务"指**租户负载运行时长**("small batch workloads … for tens of minutes",README)。
**Pod 启动速度不是 kubezoo 的指标**,属数据面范畴。

---

## 14. 诚实边界

本文的架构判断基于**源码阅读 + 各项目自身的测试报告**,不是端到端实测:

- **kubezoo + kubetron 这个组合从未运行过。** §12 的三处粘合层均为纸上推演。
- kubetron 的数据(e2e 76/76、SVC 13/13、kata-fc 5.4s Ready、Knative 冷启动 5.3s)
  是其自测结果,非本文作者测量,亦非生产验证。
- §9 的配额结论来自源码阅读:生效范围(§9.2)、objectSelector 绕过(§9.3)、
  并发超发(§9.4)均**未做运行时复现**。
- §12.3 的拒绝消息泄露面未验证。

准确表述是:**未发现结构性障碍,且工程量比 B2 小一个量级** —— 而非"已验证可行"。

---

## 参考

- kubezoo 部署形态与三方对比:[`deployment-and-comparison-cn.md`](./deployment-and-comparison-cn.md)
- kubezoo 设计:[`design.md`](./design.md) / [`design-cn.md`](./design-cn.md)
- kubetron:`/root/kubetron/README.md`、`docs/DESIGN-warm-pool.md`、`docs/DESIGN-knative-fit.md`
- Zun 与 VK provider 调研(B2 基线):任务 #86
