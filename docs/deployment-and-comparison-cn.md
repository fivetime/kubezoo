# KubeZoo 部署形态、架构拓扑与横向对比

本文回答四个问题:KubeZoo 是什么定位、怎么部署、租户身份怎么建立、以及它的限制边界在哪里。
最后把它与 Kamaji / kubegateway 放在一起对比
(⚠️ 下文的 **kubegateway** 指**上游项目**;我们自己那份 fork 现在叫
[kubesluice](https://github.com/fivetime/kubesluice),架构相同),并单列一节说明**为什么 KubeZoo 的租户
不能自带 VM 作为 worker,只能用 Virtual Kubelet**。

文中所有断言都标注了源码位置,可直接核对。

---

## 1. 定位:第四种多租户模型(KAaaS)

社区通常讲三种 Kubernetes 多租户模型:

| 模型 | 含义 | 代表 |
|---|---|---|
| NaaS(Namespace as a Service) | 给租户一个 namespace | 原生 RBAC + ResourceQuota |
| CaaS(Cluster as a Service) | 给租户一整个集群 | 各家托管 K8s |
| CPaaS(Control Plane as a Service) | 给租户一个独立控制面,数据面自备 | **Kamaji**、vcluster |

KubeZoo 提出的是第四种 —— **KAaaS(Kubernetes API as a Service)**:
**所有租户共用同一个控制面和数据面**,靠**改写请求与响应**做视图级隔离。

它的立项前提写在 README 里,三条都指向"租户多、单个小、要得急":

- **大量小租户** —— 数百个租户,每个只跑几个 Pod、运行几十分钟
- **秒级交付** —— 租户不接受等控制面启动
- **人手紧** —— 维护上千套控制面对中等规模团队不现实

由此得到它最核心的性质:**创建一个租户 = 签一张证书 + 同步几个 ClusterRole,不创建任何
Master/etcd/计算资源**。租户 ID 固定 6 位字符,理论容量 36⁶ ≈ 21.7 亿。

---

## 2. 部署形态

KubeZoo **部署在上游集群内部**,现在是**两份清单、三个组件**:

| 组件 | 清单 | 形态 | 作用 |
|---|---|---|---|
| `kubezoo` | `config/setup/proxy.yaml` | StatefulSet,示例 `replicas: 1` | 无状态代理,设计上可横向扩展 |
| `kubezoo-etcd` | 同上 | StatefulSet | **只存 KubeZoo 自己的元数据**(Tenant 对象等),前缀 `/zoo` |
| `kubezoo-controller` | **kubezoo-controller 仓库**的 `config/setup/controller.yaml` | Deployment,**只能 `replicas: 1`** | 把上游对账成 Tenant 声明的样子 |

⚠️ **两份都要装。** 只装 proxy 的话,集群会**接受 Tenant 对象然后什么都不做** ——
没有 namespace、没有 RoleBinding,而且**没有任何报错指向缺了什么**。

⭐ 为什么拆开:apiserver 是**全活**的,控制器不是。合在一起时每个 kubezoo 副本都会跑
一份控制器,三副本就是三份同时对账同一批租户。拆开之后,"要几个代理"和"要几个控制器"
才成为两个可以分别回答的问题 —— 而目前的答案分别是"可以加"和"只能 1"。

关键启动参数:

```yaml
- --etcd-prefix=/zoo
- --etcd-servers=http://kubezoo-etcd-0.kubezoo-etcd:2379

# 面向租户的服务端证书与客户端 CA
- --client-ca-file=/etc/kubezoo/pki/ca.pem
- --client-ca-key-file=/etc/kubezoo/pki/ca-key.pem      # ← 注意:CA 私钥也在这里
- --tls-cert-file=/etc/kubezoo/pki/kubernetes.pem
- --tls-private-key-file=/etc/kubezoo/pki/kubernetes-key.pem

# 连接上游集群的凭据
- --proxy-client-cert-file=/etc/upstream/pki/client.crt
- --proxy-client-key-file=/etc/upstream/pki/client-key.crt
- --proxy-client-ca-file=/etc/upstream/pki/ca.crt
- --proxy-upstream-master=https://kubernetes             # ← 集群内 kubernetes Service

- --proxy-bind-address=127.0.0.1
- --proxy-secure-port=6443
- --authorization-mode=AlwaysAllow                       # ← 见 §6 的安全说明
```

两点值得单独指出:

- ⭐ **租户 CA 的私钥不在 gateway 手里。** 签发租户客户端证书(§4)是
  **kubezoo-controller** 的活,所以私钥只挂在它那个 Pod 上;gateway 只拿 `ca.pem`
  做**验证**。⚠️ 拆分之前两件事在一个进程里,那时 KubeZoo 进程本身就是租户体系的信任根 ——
  现在信任根是 **kubezoo-controller**,攻破它等价于全体租户沦陷,攻破 gateway 不等价。
- **`--proxy-upstream-master=https://kubernetes`** —— 它连上游走的就是集群内的
  `kubernetes` Service,KubeZoo 自身是一个标准的 in-cluster 客户端。

---

## 3. 架构与拓扑

```
                租户 A kubectl          租户 B kubectl
             (OU=aaaaaa, CN=aaaaaa-admin)  (OU=bbbbbb, ...)
                      │                         │
                      └───────────┬─────────────┘
                                  │  单一入口,无需 SNI —— 租户身份来自客户端证书
                                  ▼
                    ┌──────────────────────────────┐
                    │  kubezoo  StatefulSet :6443  │  无状态
                    │  ① x509 认证 → 取出 tenantID │
                    │  ② 请求改写:加租户前缀       │
                    │  ③ 转发,带 Impersonate 头   │
                    │  ④ 响应改写:去租户前缀       │
                    └───────┬──────────────┬───────┘
                            │              │
              ┌─────────────┘              │ 改写后的请求
              ▼                            │ + Impersonate-User: <tid>-admin
     ┌──────────────────┐                  ▼
     │ kubezoo-etcd     │        ┌────────────────────────────┐
     │ prefix=/zoo      │        │  上游 Kubernetes 控制面     │
     │ 只存 Tenant 元数据│        │  apiserver / scheduler /   │
     └──────────────────┘        │  controller-manager / etcd │
                                 └─────────────┬──────────────┘
                                               │
                                        Virtual Kubelet
                                               │
                                  弹性容器服务(Fargate / ECI / ACI)
```

**两套 etcd,职责完全不同**:

- `kubezoo-etcd` —— 只存 Tenant 对象等元信息,数据量极小。
- **上游集群的 etcd —— 承载所有租户的全部业务对象。** 这是真正的规模承压点:
  N 个租户的 Pod/ConfigMap/Secret 全部落在同一套 etcd 的同一个键空间里。

数据面隔离**不由 KubeZoo 提供**。设计文档明确推荐通过 Virtual Kubelet 对接公有云弹性容器
服务,由云厂商的基础设施提供 VPC 级网络与存储隔离(§7)。

---

## 4. 租户身份与证书

### 4.1 签发

管理员创建 Tenant 对象后,KubeZoo 用自己持有的 CA 私钥为该租户签发一张客户端证书
(`pkg/util/certs.go:99-100`):

```go
OrganizationalUnit: []string{tenantID},        // OU = 6 位租户 ID
CommonName:         tenantID + "-admin",       // CN = <tenantID>-admin
```

生成的 kubeconfig 以 base64 形式挂在 Tenant 对象的 annotation 上
(`pkg/util/certs.go:37`):

```
kubezoo.io/tenant.kubeconfig.base64
```

租户自取:

```shell
kubectl get tenant 111111 --context zoo \
  -o jsonpath='{.metadata.annotations.kubezoo\.io/tenant\.kubeconfig\.base64}' \
  | base64 -d > 111111.kubeconfig
```

### 4.2 认证:租户 ID 从证书哪里来

`cmd/kubezoo/app/server.go:930` 的 `CommonNameUserConversion`:

```go
tenantIDLength := 6
if len(OrganizationalUnit) > 0 {
    if len(OrganizationalUnit[0]) == tenantIDLength && len(CommonName) > tenantIDLength {
        if OrganizationalUnit[0] == CommonName[:tenantIDLength] && CommonName[tenantIDLength] == '-' {
            u.Extra = map[string][]string{"tenant": {OrganizationalUnit[0]}}
        }
    }
}
```

三个条件必须同时成立才认定租户身份:

1. `OU[0]` 长度恰为 6
2. `OU[0]` 等于 `CN` 的前 6 位
3. `CN` 的第 7 个字符是 `-`

**不满足则不带 tenant 信息通过**(注意:是"无租户身份地通过",不是拒绝)。

### 4.3 对上游:impersonation

KubeZoo 用**一张固定的管理员客户端证书**(`--proxy-client-cert-file`)连上游,再通过
impersonation 头声明当前租户身份(`pkg/proxy/connecterproxy.go:123`、
`pkg/dynamic/simple.go:101`):

```go
req.Header[authenticationv1.ImpersonateUserHeader]  = []string{userInfo.GetName()}
req.Header[authenticationv1.ImpersonateGroupHeader] = userInfo.GetGroups()
```

这要求上游给 KubeZoo 的身份授予 `impersonate` 权限。

> **路线对比**:kubegateway 明确**放弃**了 impersonation,改用 front-proxy
> (`X-Remote-User` / `X-Remote-Group` + 前置代理专用 CA),现场验证做到全程零
> `impersonate` RBAC。两者是清晰的路线分歧,不是实现细节差异。

---

## 5. 协议转换规则

KubeZoo 的隔离能力**全部**来自一条规则(`pkg/util/util.go:146-151`):

```go
// non-crd object is namespace scoped
if isNamespaceScoped {
    return strings.HasPrefix(accessor.GetNamespace(), tenantID+"-")
}
// non-crd object is cluster scoped
return strings.HasPrefix(accessor.GetName(), tenantID+"-")
```

按资源作用域分三类改写:

| 资源类别 | 改写位置 | 例子 |
|---|---|---|
| namespace 级(Pod / Deployment / ConfigMap …,约 40+ 种) | **namespace** 加前缀 | `default` → `111111-default` |
| cluster 级(PV / Namespace / StorageClass …,约 20+ 种) | **name** 加前缀 | `pv-a` → `111111-pv-a` |
| CRD | **group** 加前缀 | `foo.example.com` → `111111-foo.example.com` |

`pkg/convert/` 下有 20 余个转换器,逐资源处理引用关系:`ownerReference`、
`objectReference`、`endpoints`、`endpointslice`、`clusterrolebinding`、`pv`/`pvc`、
`tokenreview`、`volumeattachment`、跨对象引用(`cross-object.go`)等。

**这是该架构最脆弱的部位**:任何一处引用字段漏改,都是一个跨租户可见性缺口。新增资源类型或
新增引用字段时,必须同步扩展转换器,否则默认行为可能放行。
⚠️ 尤其是 `nope.go`(什么都不做的转换器):#82 的 PersistentVolume 与 Node 两条缺口都是这个形状。
现在转换器表里**已经没有任何 kind 映射到它**,新增 kind 会落到加前缀的默认转换器 —— 这是安全的兜底。

---

## 6. 限制规则:文档说的与代码做的

这一节需要仔细读,因为**文档描述与当前实现存在偏差**。

### 6.1 上游 RBAC 不设防 —— 隔离 100% 依赖改写层

创建租户时同步到上游的 ClusterRole(`pkg/controller/controller.go:556-575`):

```go
{
    // a "root" role which can do absolutely anything
    ObjectMeta: metav1.ObjectMeta{Name: tenantId + "-" + "cluster-admin"},
    Rules: []rbacv1.PolicyRule{
        rbacv1helpers.NewRule("*").Groups("*").Resources("*").RuleOrDie(),
        rbacv1helpers.NewRule("*").URLs("*").RuleOrDie(),
    },
},
```

绑定给用户 `<tenantId>-admin`。也就是说,曾经:

> **被 impersonate 的租户身份在上游拥有 `*` on `*` 的全权。**

⚠️ **这一条已经改掉了(#87)**,但结论只改了一半,评估时要分开看:

- **namespace 级资源:现在有第二道防线**。租户的权限改为**逐 namespace 下发**
  (RoleBinding 绑在该租户的每个 namespace 上),集群范围不再有 `*` on `*`。
  跨租户的 namespace 级访问会被上游 RBAC 拒绝 —— 已带负向对照实测。
  副作用:`kubectl get <资源> -A` 对租户变成 Forbidden。
- **集群级资源:仍然没有兜底,这是结构性的**。RBAC 的 `resourceNames` 是**精确匹配**,
  表达不了"名字以 `<租户 ID>-` 开头",所以集群级资源的隔离**完全且唯一地依赖
  §5 改写层的正确性**。#82 的审计里 PersistentVolume、准入 webhook、Node 三条
  都是这条路上的实际缺口。

再加上示例部署中 KubeZoo 自身是 `--authorization-mode=AlwaysAllow`
(它把授权完全交给上游,不是漏配)。评估该方案时,**集群级资源无兜底**这一条应当排在首位。

### 6.2 Node 曾对所有租户可见(与 FAQ 冲突)—— **已修复**

FAQ 称"限制 daemonset 和 node 这类集群共享资源,租户不应感知节点",而代码里为通过
Conformance 测试开了口子,**Node 对所有租户无条件可见**:名称、标签、容量、地址,
以及 `status.nodeInfo` 里的内核 / 容器运行时 / kubelet 版本。虽是只读,但租户可据此
推断集群规模与其他租户的负载分布,也等于拿到一份"哪里有 CVE 就查哪里"的索引。

⚠️ **口子有三处,不是一处**:LIST 过滤(`pkg/util/util.go`)、Get 路径跳过名字前缀转换
(`pkg/proxy/proxy.go`)、以及 `pkg/convert/init.go` 里映射到"什么都不做"的转换器。
三处各带一条自己的 TODO 注释,**按注释文案 grep 只找得到第一处**;只删第一处的效果是
`get nodes` 变空而 `get node <名字>` 照样返回完整对象。

三处已全部移除,Node 回归成普通集群级资源。实测:list 空 / get NotFound / raw GET 404 /
watch 静默 / 平台自身不受影响。

### 6.3 DaemonSet 并未在代理层被拒绝

`cmd/kubezoo/app/apigroups.go` 中 `daemonsets` 与 `daemonsets/status` 是正常注册并
代理的资源,**在代理层没有对应的拒绝逻辑**。#87 之后上游 RBAC 虽然收紧到按 namespace 下发,
但 DaemonSet 本身是 namespace 级资源,**租户在自己的 namespace 里本来就有权建它**,
所以 RBAC 也拦不住。

**实测**:租户 `kubectl apply` 一个 DaemonSet —— 创建成功,上游 `DESIRED 1 / CURRENT 1`,
**已经在平台节点上跑起了一个 Pod**。所以不是"没拒绝"而已,是**租户可以往平台的每一个
节点上投放 Pod**。FAQ 所说的"限制 daemonset"目前是设计意图而非已实施的约束,
需要准入策略层补上。

### 6.4 版本上限 —— **已抬到 1.36**

原文记录的是移植前的状态(`go.mod` 锁 1.24、Go 1.18)。现在 `k8s.io/*` 全族锁定
**1.36.3**(staging 模块 `v0.36.3`),Go 基线 1.26.0,生成代码已按 1.36 重新生成,
`make verify-codegen` 会校验签入产物是否一致 —— ⚠️ **它和被生成的代码一起在 kubezoo-contract**,不在本仓库。

⚠️ 仍然成立的那一半:KubeZoo **依旧引用 `k8s.io/kubernetes` 的内部包**
(`pkg/apis/core` 等)并 fork 了 CRD handler,所以**跨小版本升级仍是一次有意的移植,
不是改个版本号**;§5 的转换器同样需要针对新版本的资源与字段同步扩展。

---

## 7. 为什么租户不能自带 VM 作为 worker

这是 KubeZoo 与 Kamaji 最本质的能力差异,原因是结构性的,不是"尚未实现"。

### 7.1 kubelet 需要的视图,KubeZoo 表达不出来

kubelet 的核心 watch 是 `pods?fieldSelector=spec.nodeName=<自己>`,这是一个
**跨 namespace、跨租户的横切视图**。而 KubeZoo 的隔离维度是"namespace 以 `<tenantID>-`
开头"(§5)。两个维度正交,经过 KubeZoo 只有两种结果:

- 按租户过滤 → kubelet 看不到调度到它上面的其他租户的 Pod → **那些 Pod 永远起不来**
- 不按租户过滤 → **隔离当场失效**

因此 **kubelet 在原理上不能经过 KubeZoo**,只能直连上游 apiserver。

### 7.2 直连上游后,这台 VM 不再属于任何租户

Node 是 cluster-scoped 资源。VM 注册进上游共享集群后,调度器会把**任何租户**的 Pod 调度上去
—— 没有任何机制把 Node 绑定到租户。要做绑定就需要 taint + 为每个租户的 Pod 注入
toleration 与亲和性,那是另一套隔离机制,KubeZoo 的转换层并不提供。

### 7.3 安全:租户掌控的 kubelet 是共享集群的越权入口

kubelet 有权读取**调度到本机的所有 Pod 的 Secret 与 ServiceAccount token** —— 这是合法行为,
`NodeRestriction` 限制的是别的事情。租户只要设法让其他租户的 Pod 调度过来,即可读到对方凭据。

Kamaji 不存在这个问题:每租户独立 apiserver 与独立数据,租户的 kubelet **物理上看不到**
其他租户的对象。

### 7.4 Virtual Kubelet 恰好绕开上述三条

- **由平台运行,不是租户运行** → 凭据不外流(解 7.3)
- **不对应真实机器**,Pod 转发给弹性容器服务,每个 Pod 一个独立沙箱/微 VM,不共享内核 →
  隔离从"节点级"下沉到"Pod 级"(解 7.2)
- 租户完全不需要感知节点,容量表现为近乎无限(解 7.1)

**一句话:Virtual Kubelet 不是"节点的一种实现",而是把"节点"这一概念整体替换为"按 Pod
计费的弹性容器",从而不再需要节点级隔离。**

---

## 8. 真实 Worker Node vs Virtual Kubelet Node

| 维度 | 真实 Worker Node | Virtual Kubelet Node |
|---|---|---|
| **API Server 注册** | 注册为标准 Node 对象,同步真实硬件状态 | 同样注册为 Node 对象,状态由 VK 模拟并汇报;**在 API 层与真实节点不可区分**,这正是其设计目的 |
| **容量(Capacity)** | 反映物理/虚拟机的**真实** CPU、内存上限 | **虚构值**(如固定 10000 Core 或动态上报),真正的上限是云厂商配额,**调度器完全看不见** |
| **Pod 调度** | 标准 kube-scheduler 做 CPU/内存/拓扑约束计算 | **调度路径与普通节点完全一致,没有任何旁路**。区别仅在于所依据的容量是虚构的。<br>Taint 的作用是**排斥**普通 Pod,Toleration 只表示"允许",**都不产生调度偏好**;要把 Pod 引导过来必须另用 `nodeSelector` / `nodeAffinity`(如 `type: virtual-kubelet`)或云厂商的注入机制 |
| **失败点位移** | 资源不足在**调度阶段**失败,Pod 干净地 `Pending` | 调度**成功**,VK 调云 API 时才失败;表现为大量 Pod 卡在 `ContainerCreating`,排障体感差 |
| **Pod 生命周期与 IP** | kubelet 调用 containerd/CRI 本地创建容器,IP 来自节点 CNI | VK 转换后调用无服务器/远程 API(ACI、ECI、Fargate),**Pod IP 来自底层云网络** —— 集群内路由到该 IP 需要云网络可达(通常要求同 VPC) |
| **kubelet API 面**<br>(logs / exec / port-forward / metrics) | kubelet 原生提供,完整 | **必须由 VK 自行实现**,各 provider 覆盖参差且常不完整(`port-forward`、`kubectl top` 经常缺失)。运维上最容易被低估的一项 |
| **DaemonSet 支持** | 默认运行(日志采集、监控、CNI 插件等) | **默认无法工作**。DaemonSet 控制器只为内置节点状况污点自动加容忍(`not-ready` / `unreachable` / `disk-pressure` / `memory-pressure` / `pid-pressure` / `unschedulable` / `network-unavailable`,见 k8s `pkg/controller/daemon/util/daemonset_util.go`),**不容忍 VK 的自定义污点**。<br>更根本的是**语义为空**:VK 后面没有宿主机,"每台机器一份"无从谈起 |
| **DaemonSet 失效的实际代价** | — | 日志采集、监控 agent、CNI、安全/合规 agent、node-exporter **全部失效**,只能改为每 Pod 塞 sidecar(成本 × Pod 数)或改用云厂商托管等价物(供应商锁定)。迁移工作量常被严重低估 |
| **节点级能力** | hostNetwork / hostPath / privileged / device plugin / NUMA / sysctl 均可用 | **基本全部不可用**;GPU 等需由 provider 单独提供规格 |
| **Pod 启动延迟** | 镜像预热后**秒级** | 冷启动**数十秒**(拉镜像 + 起沙箱);短任务场景中可能占总时长过半 |
| **节点健康检查** | 基于本地资源与 heartbeat。通信中断 → `NotReady` → 触发 Pod 重调度 | 基于远程云服务 API 状态。VK 与云 API 断联 → 停止续 node lease → `NotReady` → node-lifecycle-controller 打 `NoExecute` 污点 → **该节点上 Pod 被批量驱逐** |
| **断联的真正后果** | 影响该节点上数十个 Pod | ⚠️ **裂脑**:API 中 Pod 已被删除,但 ECI/Fargate 里的容器**毫不知情,继续运行、继续计费**;若上层控制器重建,同一份工作负载会**跑两遍**(对批任务尤其致命) |
| **故障域大小** | 一个节点 ≈ 几十个 Pod | **一个 VK 节点常承载上千 Pod**,配合上一行的驱逐机制,是整套架构最大的单点 |

**这张表的主线**:VK 在 API 层伪装成节点,在语义层却把"节点"整个抽空了。凡是依赖"节点是一台
真实机器"这一前提的东西 —— DaemonSet、hostPath、节点级 agent、基于节点的故障域假设 ——
都会在此失效。

---

## 9. 与 Kamaji / kubegateway 的横向对比

三者**不是竞品**,处在不同的层:

| | **KubeZoo** | **Kamaji** | **kubegateway** |
|---|---|---|---|
| 多租户模型 | KAaaS | CPaaS | 不做租户,做**路由 / 限流** |
| 租户得到什么 | 一个受限的 API 视图 | **完整的独立集群** | 取决于其后端挂什么 |
| apiserver | **全租户共用 1 个** | 每租户 1 个 | 前端挂 N 个 |
| etcd | **全租户共用 1 套键空间** | 共享 datastore,每租户独立前缀 | 不涉及 |
| 隔离手段 | **改写 namespace / name / group** | 真正的控制面隔离 | SNI / Host 路由 |
| 隔离强度 | 视图级(共享控制面 + 数据面 + 节点) | 控制面完全隔离 | 不提供隔离 |
| 上游认证 | **impersonation** | — | **front-proxy(X-Remote-\*)** |
| 租户识别 | 客户端证书的 **OU** | 各租户独立控制面 | **SNI 主机名** |
| 创建租户成本 | 签一张证书,**秒级** | 起一套控制面 | 增加一个 UpstreamCluster |
| 租户能否自带 worker | ❌ 结构上不行(§7) | ✅ | ✅ |
| 每租户限流/熔断 | ❌ 无 | 需外部提供 | ✅ 内建 |
| 适用场景 | 数百个只跑几个 Pod 的小租户 | 需要完整集群语义的租户 | 需要跨集群路由与配额治理 |

**可以叠加**:小租户交给 KubeZoo 压进一个集群,大租户用 Kamaji 单开控制面,kubesluice 挡在
apiserver 前做路由与限流。三者解决的是不同问题。

---

## 10. 选型时的关注点小结

1. **隔离强度**:视图级。§6.1 更正后的结论分两半 —— **namespace 级资源现在有上游 RBAC
   作为第二道防线**(#87,按 namespace 下发),**集群级资源没有,而且是结构性的**
   (RBAC 的 `resourceNames` 精确匹配,表达不了名字前缀)。集群级那半完全依赖改写层的正确性。
   适合**互相之间半可信**的租户(如同一公司内部团队),不适合**互不信任**的租户。
2. **规模承压点在上游 etcd**:所有租户对象共用一套键空间。租户数量上去之后,
   etcd/apiserver 的规模能力会先于其他一切成为瓶颈。
3. **无每租户限流**:一个租户打爆共享 apiserver 会影响全体,项目本身不提供配额治理手段。
   ⚠️ 自带的 ClusterResourceQuota 也不解决这个:实测它**每个 namespace 各发一份完整额度**
   (声明 4 core、6 个 namespace 就是 24 core),且是 `replicas: 1` + `failurePolicy: Fail`
   的单点。
4. ~~**版本停在 1.24**~~ —— 已移植到 **1.36.3**(§6.4)。仓库上游活跃度低这一点仍然成立,
   §5 转换器的扩展仍需持续跟进。
5. **数据面隔离是外部依赖**:必须搭配 Virtual Kubelet + 云厂商弹性容器才成立,
   §8 列出的全部代价都要一并接受。
6. **待修项**:§6.3 的 DaemonSet 未拒绝(实测租户能在平台节点上投放 Pod);
   `runtimeClassName` / `ingressClassName` / `priorityClassName` 三个引用字段不改写,
   租户可引用平台的同名对象(含 `system-cluster-critical`)。
   ~~§6.2 的 Node 无条件可见~~ 已修复。

---

## 参考

- 设计文档:[`design.md`](./design.md) / [`design-cn.md`](./design-cn.md)
- 部署步骤:[`manually-setup.md`](./manually-setup.md) / [`manually-setup-cn.md`](./manually-setup-cn.md)
- FAQ:[`faq.md`](./faq.md) / [`faq.zh.md`](./faq.zh.md)
- 部署清单:[`../config/setup/proxy.yaml`](../config/setup/proxy.yaml)
