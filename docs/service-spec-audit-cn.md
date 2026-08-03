# ServiceSpec 逐字段判定:谁在守,守得住吗

与 `PersistentVolumeClaimSpec`、`PodSpec` 两轮同一套做法。判定针对 **kubezoo 1.36 目标版本**。

---

## 0. 起点:**kubezoo 与策略层都不守 Service**

- `pkg/convert/` 里**没有 `service.go`** —— Service 只走 `DefaultConvertor`(namespace 加前缀、
  ownerReference 转换),spec 原样透传
- `config/policy/` 里**没有一条规则的 `kinds:` 包含 Service**

⭐ **但数据面上 kubetron 是有份的,必须分清它管到哪里** —— 目标架构(§B1)里数据面多租户
由 kubetron(Neutron/OVN + Octavia)负责:

| | kubetron 管吗 | 依据 |
|---|---|---|
| `type: LoadBalancer` | ✅ **管** —— 去开 Octavia LB | `kubetron/pkg/service/reconciler.go:395` |
| `loadBalancerIP` / `loadBalancerClass` | ✅ 语义归它 | 同上,LB 的实现方 |
| **`externalIPs`** | ❌ **完全不碰** | `kubetron/pkg/service/` 全目录零引用 |
| 其余字段 | ❌ | |

⇒ `externalIPs` 落在**主网络的 Service 代理**(kube-proxy 或 CNI 的等价实现)上,
kubetron 不接管,kubezoo 不校验,策略不匹配 —— **没有任何一层在守**。

---

## 1. 判定表

| 字段 | 判定 | 说明 |
|---|---|---|
| **`externalIPs`** | ⛔⛔ **逃逸** | 见 §2① —— 跨租户 + 打平台的流量劫持 |
| `type: LoadBalancer` | ⚠️ 成本/暴露 | 真的去云上开一个 LB。是平台卖的东西,不是租户随手拿的 |
| `loadBalancerIP` | ⚠️ | 向 provider 点名要某个 IP,可能是别人预留的 |
| `loadBalancerClass` | ⚠️ 同构 | 决定**哪个 LB 控制器**接管 —— 与 `storageClassName` / `ingressClassName` 同一个形状 |
| `ports[].nodePort` | ⚠️ 占位 | 在**每个节点**上钉一个端口;先到先得,可占住别人想要的 |
| `healthCheckNodePort` | ⚠️ 同上 | 同一形状 |
| `clusterIP` / `clusterIPs` | 低 | 可点名,但分配器不让撞已分配的 ⇒ 只能拿空闲的 |
| `externalName` | 低 | CNAME 别名,只影响解析到该租户 namespace 里这个名字的客户端 |
| `selector` | ✅ 天然 | namespace 内解析,跟着前缀走 |
| `loadBalancerSourceRanges` | ✅ | 自我限制,只会更严 |
| `ports`(其余)/ `sessionAffinity*` / `ipFamilies` / `ipFamilyPolicy` / `internalTrafficPolicy` / `externalTrafficPolicy` / `trafficDistribution` / `publishNotReadyAddresses` / `allocateLoadBalancerNodePorts` | ✅ 无关 | |

---

## 2. 结论

### ⛔⛔ ① `externalIPs`:租户可以劫持任意 IP 的流量

一个 Service 写上 `externalIPs: [<任意 IP>]`,**主网络的 Service 代理会在每个节点上把发往那个
IP 的流量 DNAT 到这个租户的 Pod**。租户不需要拥有那个 IP,也没有任何所有权校验。

⚠️ 具体由哪个组件实现取决于主网络用什么(kube-proxy / CNI 的 kube-proxy 替代)——
**但不取决于租户 Pod 挂在哪个 OVN 网络上**:DNAT 发生在**节点的网络命名空间**里,
凡是流量经过节点就会被劫走。kubetron 给的是 Multus 二级网卡,改变不了这一层。

能截什么:

- **另一个租户的服务** —— 只要知道对方的 external IP
- **平台自己的东西** —— apiserver、DNS、数据库、镜像仓库的 IP
- **集群外的任意地址** —— 集群内所有 Pod 发往该 IP 的流量都会被引到租户的 Pod 上

⭐ 这是 Kubernetes 公认的问题(**CVE-2020-8554**),官方缓解就是准入插件
`plugin/pkg/admission/network/denyserviceexternalips`。

⭐ 而在目标架构里,**租户接入公网的正路是 `type: LoadBalancer`(经 kubetron/Octavia)**,
`externalIPs` 没有任何正当用途 ⇒ 在 kubezoo 层按租户身份拒掉它,与架构一致。

⚠️ **但那个插件是"对所有人一刀切"**,而平台自己可能有正当用途 —— 这正是该由 kubezoo 这层
按租户身份来拒的原因。

⭐ **规则沿用官方插件的,不自己发明**:`isSubset(new, old)` —— **允许保留或缩小,不允许新增**。
所以存量 Service 不会因为这次加固变得不可写,而租户也加不进新的。

### ⚠️ ② LoadBalancer 一族:成本与"哪个控制器"

`type: LoadBalancer` 会真的去开一个云上负载均衡器(**要钱、给公网 IP**);`loadBalancerClass`
决定哪个控制器接管,与 `storageClassName` 完全同构。

⛔ **不该由 kubezoo 做,这是 kubetron 的地盘。** `type: LoadBalancer` 的实现方是
kubetron 的 service reconciler(去开 Octavia LB),配额、计费、IP 归属这些问题都在那一侧
有上下文,kubezoo 这里没有。

曾经看着可行的一条是:把 `loadBalancerClass` 当成第四个 class 走 `pkg/publishedclass`
(标签发布 + 未发布即拒)—— 毕竟它和 `storageClassName` 长得一模一样。

⛔ **已核实,这条走不通:kubetron 既不读 `loadBalancerClass` 也不读 `loadBalancerIP`**
(`kubetron/pkg/` 非测试代码里两者均零引用)。它接管一个 Service 的依据是注解
`kubetron.network.kubevirt.io/lb: "true"`(`pkg/service/reconciler.go:117`),
连 `type: LoadBalancer` 都不是。**在 kubezoo 层校验一个下游根本不看的字段,是自欺。**

⭐ 这条留作教训:**"下游读不读这个字段"必须查源码,不能按"和 X 同构"推。**
本轮就是靠这一步否掉了一个已经想好怎么做的方案。

### ⚠️ ③ nodePort 一族:先到先得的端口占位

`ports[].nodePort` 和 `healthCheckNodePort` 让租户在**每个节点**上钉住一个具体端口。
分配器会防撞,所以这不是劫持,是**占位**:一个租户可以把别人想要的端口先占掉,
也可以把自己的服务钉在一个约定俗成的端口上(比如 30080)让外部误以为是平台的。

**没有做**。真要管,合理的形状是"租户不得点名 nodePort,只能由系统分配"——
但这会打断任何依赖固定 nodePort 的存量租户,同样是产品决策。

⚠️ 而且要先核实一件事(**未核实**):在 kubetron 的 OVN 数据面下,nodePort 还是不是
一条有效的对外暴露路径。若租户流量根本不经过节点端口,这条的严重性会大幅下降 ——
别在没确认之前就按"标准 Kubernetes"的直觉去加校验。
