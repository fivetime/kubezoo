# 运营手册:kubezoo + 策略层怎么配合使用

> 这份文档给**运营 / SRE**。工程侧的推导和实测记录在
> `isolation-audit-cn.md` 与 `kaaas-platform-architecture-cn.md`,这里只讲**怎么做、怎么验**。

## 0. 一句话:kubezoo 单独装上,隔离是不完整的

⭐ **架构前提:kubezoo 是租户到控制平面的唯一入口。** 这是一个共享控制平面的
多租户平台,租户负载在自己的网络里(kubetron / OVN),**上游 apiserver 不在它们的
可达范围内** —— 租户碰不到控制平面,只能经 kubezoo。下面所有判断都建立在这条上;
它是**部署要求**,不是代码能保证的东西,见 §1.2。

kubezoo 是租户的**前门**。但**前门不做校验**:`tenantProxy` 丢弃全部
`ValidateObjectFunc`,所以 **kubezoo 从不跑准入** —— 它翻译名字、约束它认识的字段,
其余一概原样转发。⇒ 下面这套策略**不是可选加固,是隔离的一半**,
因为**没有别的东西在做准入**。

⚠️ **这一节的理由改过一次,结论没变,值得知道为什么。** 早先写的是"租户的 Pod
直连上游、完全不经过 kubezoo,所以要策略层兜底"——那在**没有网络隔离**的部署里是实测事实
(审计 §S)。现在的部署形态把那条路堵上了,但**结论不变**:策略层依然承重,
只不过承的是"kubezoo 不做准入"这件事,而不再是"有一条绕过前门的路"。
⛔ 别把前提的变化读成"策略层可以省了"。

**没装策略层的集群,租户可以**(每条都实测过):
跑出 kata 沙箱、拿全集群最高优先级抢占、建 privileged + hostNetwork 的 Pod、
往每个节点投放 DaemonSet、把 Pod 钉到任意节点、把 Pod 绑到别的租户的节点上。

---

## 1. 部署:必须装什么

```bash
kubectl apply -f ../kubezoo-contract/config/policy/   # ⚠️ 策略在 contract 仓库
```

装完确认**两类都在**:

```bash
kubectl get clusterpolicy                    # Kyverno 的,READY 必须全是 True
kubectl get validatingadmissionpolicy        # 原生的,get clusterpolicy 看不到它们
```

⚠️ **两个占位符,部署前必须替换**,否则策略要么拒掉一切、要么什么都不管:

| 文件 | 占位符 | 换成 |
|---|---|---|
| `tenant-ingress-hostnames.yaml` | `TENANT_DOMAIN_SUFFIX` | 平台给租户分配子域的后缀 —— **不换的话租户的 Ingress 全被拒** |
| `tenant-api-endpoint.yaml` | `KUBEZOO_ADDRESS_PLACEHOLDER` | kubezoo 在集群内的可达地址 |

⚠️ **发给租户的凭据一律不得携带 `kubezoo:proxied:*` 组。**
租户的集群级权限挂在这个组上,而不是挂在租户的用户名上 —— 这样它**只在 kubezoo 转发的
请求上存在**。集群级资源没有 RBAC 兜底(`resourceNames` 表达不了 `<租户ID>-` 前缀),
所以一旦某个租户凭据带上这个组,它就能列举并**删除**所有租户的 IngressClass / CRD /
PV / ClusterRole。签发租户证书时必须由平台钉死 subject,不能照抄租户 CSR 里的 O。
同理:**不要用上游集群信任的 CA 去签租户证书** —— kubezoo 用自己的 CA 是承重设计。

⚠️ **`kubezoo:role-author` 这个组同理,而且更要紧。** 它对应的 ClusterRole 只有一条
`escalate` on `clusterroles` —— 那是 RBAC 提权检查的**免检通道**。kubezoo 只在租户写
ClusterRole 时断言它,写对象的动词仍来自租户自己的角色,所以组本身写不动任何东西。
但**任何持有它、同时又能 create clusterroles 的身份,可以写出任意内容的 ClusterRole**。
和上面一样:发给租户的凭据不得携带任何 `kubezoo:` 开头的组。

⚠️ 还有一个**不在策略里、在对象的标签上**的:平台自己的 IngressClass / StorageClass
默认对租户**完全不存在**。要让租户能用,给对象打标签:

```bash
kubectl label ingressclass nginx    ingressclass.kubezoo.io/published=true
kubectl label storageclass fast-ssd storageclass.kubezoo.io/published=true
```

- **不打的话租户完全无法接入公网**(所有 class 都会被前缀化成租户私有的),
  StorageClass 那边则是 `kubectl get storageclass` 空空如也 —— 引用其实是通的
  (`spec.storageClassName` 原样透传),但租户**没法知道有哪些名字可用**。
- 打错则等于把公网入口的名字告诉错人。
- **标签即时生效,不用重启网关。** 这点是承重的:网关是单副本 StatefulSet,
  重启一次 = 所有租户的 API 中断 + 所有租户 operator 的 watch 断掉,
  只为了改一行配置。老的 `--public-ingress-classes` / `--public-storage-classes`
  两个 flag 仍然生效(与标签取**并集**,升级不会突然什么都看不见),但它们只能靠
  重启修改,而且**拼错是完全静默的** —— 名字就是不出现,没有报错也没有日志。

⛔ **升级前必做:先盘点,再升级。** 发布现在是**授权**,不只是能否发现 ——
**未打标签的存储类,租户建不了新 PVC**。升级前没把在用的类打上标签,存量租户会当场
建不了 PVC(已有的 PVC 不受影响,照常供给)。

```bash
# 盘点:全集群 PVC 实际在用哪些存储类
kubectl get pvc -A -o jsonpath='{range .items[*]}{.spec.storageClassName}{"\n"}{end}' \
  | grep -v '^$' | sort -u
# 把上面每一个都打上标签,然后再升级
```

**三态**,不是开关:

| 标签 | 租户看得见 | 能否新建 PVC |
|---|---|---|
| 无标签 | ❌ | ❌ |
| `=true` | ✅ | ✅ |
| `=deprecated` | ✅ | ❌(但看得见,知道为什么) |

⭐ 留空 `storageClassName` **永远不会被拒** —— 那是在要默认存储类,由上游的 `setdefault`
准入插件填,而默认类是平台自己选的。大多数 PVC 都不写类名,拒掉它们等于全线停摆。

⛔ **由此带来一个必须知道的空隙:`setdefault` 跑在 kubezoo 之后。** 所以留空的 PVC 会落到
带 `storageclass.kubernetes.io/is-default-class` 注解的那个类上 —— **不管它有没有发布、
是不是 `deprecated`**。要下线默认存储类,**光打 `deprecated` 标签不够,必须同时把 default
注解摘掉**,否则新 PVC 照样源源不断落上去:

```bash
kubectl label      storageclass old-default storageclass.kubezoo.io/published=deprecated --overwrite
kubectl annotate   storageclass old-default storageclass.kubernetes.io/is-default-class-       # 别忘了这条
kubectl annotate   storageclass new-default storageclass.kubernetes.io/is-default-class=true
```

⭐ **`deprecated` 而不是直接摘标签**:两者都会拦住新 PVC,区别在于 `deprecated`
**仍然可见**,租户能看到这个类存在、正在下线,从而解释自己已有的 PVC 为什么引用它;
摘标签则是直接消失。已绑定 PVC 的 `spec.storageClassName` 是不可变字段,租户连自己的
manifest 都改不了,所以下线一个存储类必须给迁移窗口,而不是让某个 StatefulSet 扩容时
突然失败。**两种情况下已存在的对象都不受影响。**

⚠️ **摘标签不再是安全可逆的动作**了(以前是)。手滑摘掉一个在用的类 = 该类的新 PVC 立刻
全拒。要下线走 `deprecated`,不要直接摘。

⚠️ IngressClass 这边**不需要**同样的拦截:未发布的 ingress class 不是被拒,而是被
**前缀化进租户自己的名字空间**,只能由租户自己跑的控制器来服务 —— 机制不同,本来就是安全的。

### ⭐ VolumeAttributesClass:同一套机制,但默认什么都不发布

`spec.volumeAttributesClassName` 装的是 CSI 驱动的 IOPS / 吞吐参数 —— 它是**平台卖的性能档**,
不是租户随便挑的东西。这个字段在 1.36 是 **GA + LockToDefault**(关不掉),而在此之前
kubezoo **完全不校验它**,租户写一个平台内部的 VAC 名字就能拿到那档性能。

```bash
kubectl label volumeattributesclass gold volumeattributesclass.kubezoo.io/published=true
```

与存储类的**三点差异**:

1. **默认一个都不发布**,而不是靠 flag 兼容 —— 因为以前压根没有校验,没有存量行为要兼容。
   不打标签 = **租户根本设不了这个字段**,这正是"平台卖的东西"该有的默认。
2. **这个字段可变**(存储类不可变)。租户可以在已绑定的 PVC 上改档位 —— 所以校验
   **不能只拦 CREATE**,UPDATE 也拦。
3. 但 **只在值真的改变时拦**。不碰这个字段的写入照常放行,否则 GitOps 重放一份没变的
   manifest 会永远失败 —— 和存储类那条铁律是同一个道理。

⭐ 因此**撤掉一个 VAC 的标签,不会影响已经在用它的 PVC** —— 只有新引用和改档位会被拒。

⚠️ 与存储类不同,这里留空是**真的"不套用任何档位"**,没有 `setdefault` 那样的上游插件会替它填。

⛔ **`READY=<none>` 不是"还在同步",是"这条策略什么都没在做"。**
实测发生过:一条我们自己的 VAP 拦住了 Kyverno 注册自身 webhook 所需的写入,
于是三条策略永远不就绪,**`pods` 的 webhook 根本没注册**,
租户的 `hostNetwork` / `hostPID` / `nodeName` 全部放行 ——
而屏幕上只有那几个 `<none>`。**看到 `<none>` 就当故障处理,先查 Kyverno admission 控制器日志。**

### 1.0 ⭐ 最可靠的一步:跑验证套件

```bash
hack/lab/verify.sh          # 每条断言都提交一个必须被拒的东西,再看它究竟被谁拒
```

上面那些 `get` 只能告诉你"对象在";**只有这个能告诉你"它真的会拒"**。

⚠️⚠️ **kubezoo-contract** 的 `config/policy/` 里有**三条**是 Kubernetes 原生的 `ValidatingAdmissionPolicy`,
不是 Kyverno** —— `kubectl get clusterpolicy` **看不到它们**。
只查 Kyverno 就以为装全了,是最容易犯的错。

| 文件 | 引擎 | 不装的后果 |
|---|---|---|
| `tenant-platform-classes.yaml` | Kyverno | 租户可跑出沙箱、可抢占全集群 |
| `tenant-pod-security.yaml` | Kyverno | 租户可建 privileged + hostNetwork Pod |
| `tenant-deny-daemonset.yaml` | Kyverno | 租户可往每个节点投放 Pod |
| `tenant-scheduling.yaml` | Kyverno | 租户可绕过调度器钉节点 |
| `tenant-placement.yaml` | Kyverno | 租户可自选落点、跑到别人的节点池 |
| `tenant-deny-binding.yaml` | **原生 VAP** | 租户可把 Pod 直接绑到别的租户节点 |
| `tenant-frozen-deny-writes.yaml` | **原生 VAP** | 冻结的**上游那一半**。前门的 `WithTenantSuspension` 在缓存里找不到租户时**放行**(故意的,靠上游兜底)—— 没有这条,那次放行就没有兜底 |
| `tenant-ingress-hostnames.yaml` | **原生 VAP** | 任何租户可抢任何主机名,**先到先得、落败方零报错** |
| `tenant-own-namespace-name.yaml` | Kyverno | Pod 从 downward API 拿到**上游** namespace 名 ⇒ 经 kubezoo 二次前缀,**operator 读自己的 namespace 全部 NotFound** |
| `tenant-api-endpoint.yaml` | Kyverno | 租户负载的 in-cluster 客户端**指不到 kubezoo** ⇒ 在 §1.2 的网络形态下它谁也连不上,operator 全废;网络没隔离时则是直连上游、看不见自己的 CRD 组 |

### 1.1 ⚠️ 装完必须做一次存量修正

**策略只在准入时生效,对已经存在的东西一律不追溯。**

```bash
# 存量 namespace 的 PSA 标签(策略装上之前建的不会被自动钉回)
kubectl label ns -l kubezoo.io/tenant \
  pod-security.kubernetes.io/enforce=restricted --overwrite

# 存量违规 Pod:必须自己找出来删掉
# 实测:补完 namespace 标签后,已经在跑的 privileged Pod 仍然 Running,只发一条 warning
# ⚠️ 按 kubezoo.io/tenant 标签圈定范围,不要按 namespace 名字前缀猜 ——
#    平台自己的 namespace(kyverno / ingress-nginx / local-path-storage)会被误报
for ns in $(kubectl get ns -l kubezoo.io/tenant -o jsonpath='{.items[*].metadata.name}'); do
  kubectl -n "$ns" get pods -o json | jq -r --arg ns "$ns" '
    .items[] | select(.spec.hostNetwork == true or .spec.hostPID == true or
      (.spec.containers[]? .securityContext.privileged == true))
    | "\($ns)/\(.metadata.name)"'
done
```

**不删就等于没封。**

### 1.2 ⛔⛔ 租户网络必须够不到上游 apiserver —— 这条是承重前提

⭐ **这是整套隔离的地基,而且它不在代码里。** 平台的判断建立在"租户到控制平面只有
kubezoo 一条路"上;这条路之外若还有第二条,前门上的每一条约束都可以被绕过。

**为什么代码保证不了**:租户 Pod 的 in-cluster 客户端读的是 `KUBERNETES_SERVICE_HOST`,
而 `tenant-api-endpoint` 策略是**准入时把它改道**到 kubezoo(podspec 同名 env 压过
kubelet 注入的那个)。**改道不是阻断** —— 上游 apiserver 若在网络上仍然可达,
一个没被策略覆盖的 Pod(策略挂了、Pod 早于策略创建)拿着投影 SA token 照样连得上。

⇒ **必须在网络层保证**。本平台的做法是租户负载跑在自己的网络里(kubetron / OVN),
上游 apiserver 不在其可达范围。若你的部署形态不同,等价手段是 NetworkPolicy /
CNI 策略,把租户 namespace 到 apiserver 的出向流量断掉。

```bash
# 自查:从一个租户 Pod 里,上游必须连不上,kubezoo 必须连得上
kubectl -n <租户ID>-default exec <pod> -- \
  sh -c 'curl -sk -m3 https://kubernetes.default/version >/dev/null && echo "⛔ 上游可达" || echo "✅ 上游不可达"'
kubectl -n <租户ID>-default exec <pod> -- \
  sh -c 'curl -sk -m3 https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT/version >/dev/null && echo "✅ kubezoo 可达" || echo "⛔ kubezoo 不可达"'
```

⚠️ **这条破了会静默地破**:租户的东西照常工作(它本来就该经 kubezoo),坏掉的只是
"经不经都行"这件事,而没有任何告警会说这话。**纳入变更评审**:动 CNI、动 kubetron
数据面、加节点、改 NetworkPolicy 之后都要重跑上面两条。

### 1.3 ⛔⛔ 配额组件必须**先于**租户控制器启动 —— 顺序错了会静默失效

租户级配额(`Tenant.spec.quota`)由两个独立进程接力,而**接力棒只递一次**:

```
Tenant.spec.quota → kubezoo-controller 建 ClusterResourceQuota
                  → 配额组件给每个租户 namespace 派生 ResourceQuota + 汇总用量
                  → 准入 webhook 在 Pod CREATE 时按租户总量判定
```

`kubezoo-controller` 在**启动时**问上游一次"你提供 `quota.kubezoo.io` 吗",
拿到否就把配额客户端永久置空,**之后不再重试**。而 `quota.kubezoo.io` 的 CRD 是**配额组件**
自己启动时装的。所以:

> **先起租户控制器、后起配额组件 ⇒ 该控制器整个生命周期内,所有租户的 `spec.quota` 全部被忽略。**

`kubectl get tenant` 看着完全正常,租户侧也没有任何报错 —— **它只是想建多少建多少**。
唯一的痕迹在 `kubezoo-controller` 的日志里,有两条,**要找的是第二条**:

| 级别 | 内容 | 含义 |
|---|---|---|
| INFO(启动时一次) | `the upstream cluster does not serve clusterresourcequotas` | 只说明本集群没装配额组件 —— **没有租户用配额时这是正常的** |
| **ERROR(每个受影响租户一条)** | `tenant <id> sets spec.quota but NO QUOTA IS BEING ENFORCED for it` | **有租户设了 `spec.quota` 却完全不生效**,这条才是故障 |

所以巡检 grep `NO QUOTA IS BEING ENFORCED`,不要 grep 那条 INFO。

**正确顺序**:先 `kubectl apply -f config/setup/quota.yaml`,等 CRD 就绪,再起(或重启)
`kubezoo-controller`。已经装反了的集群,**重启一次 kubezoo-controller 即可**,不需要重建租户。

自查(应当每个有 `spec.quota` 的租户都有一个):

```bash
kubectl get clusterresourcequota
```

⚠️ 另外两条,来自这条链路的实际形态:

- **每个 namespace 里派生的 `ResourceQuota` 带的是"全额"**,不是均分。跨 namespace 的总量
  **只由 webhook 保证**(它在判定前把租户级的汇总用量顶替进去)。所以
  **webhook 没装 = 租户拿到 `额度 × namespace 数`**,而每个 namespace 单看都是合规的。
- webhook 是 `failurePolicy: Fail`,且只排除了 `default` / `kube-system`。
  **配额组件挂掉 = 全集群(除这两个 namespace)Pod 创建全部失败**,报错是连接被拒,
  与配额无关,极易误诊。它是单点,排空节点时**不要和租户负载一起赶**。

---

## 2. 节点池约定

每个租户一个 worker 节点池,节点上打**该租户专属**的标签和同值污点:

```bash
kubectl label node <node> kubezoo.io/pool=<租户ID>
kubectl taint node <node> kubezoo.io/pool=<租户ID>:NoSchedule
```

### ⭐⭐ 标签必须每租户专属 —— 这是承重前提,不是优化

不要用 `node-role.kubernetes.io/worker` 这类**所有池子共有**的标签。原因很反直觉:

| | 调度器 | kubelet |
|---|---|---|
| `NoSchedule` 污点 | 检 | **不检** |
| `nodeSelector` | 检 | **检** |

租户可以绕过调度器直接绑定 Pod(见 §3 的 `tenant-deny-binding`),那条路上
**污点是失效的,只有 nodeSelector 拦得住**。

**实测**:标签专属时,跨租户绑定的 Pod 被 kubelet 拒(`NodeAffinity failed`,容器一个没起);
把目标节点也标成同一个池子,**同一次绑定就让 Pod 真的 Running 在了别的池子上**。

### ⛔⛔ 平台自己的节点**也必须打污点** —— 决定了 Kyverno 挂掉时会发生什么

租户 Pod 的 `nodeSelector` 和 `tolerations` **都是 Kyverno 注入的**,而 Kyverno 是
webhook,是单点。它不在的时候,Pod 到达 apiserver 时**既没有 nodeSelector 也没有容忍**。
那样一个 Pod 会落到哪,完全取决于集群里**有没有不带污点的节点**:

| 平台节点 | Kyverno 挂掉时,无容忍的租户 Pod |
|---|---|
| **不打污点** | ⛔ 它唯一能落的地方就是**平台节点** —— 控制面、系统组件所在的机器 |
| **打了污点** | ✅ 哪儿都落不了,**停在 Pending** |

```bash
# 平台/系统节点(含控制面)全部打上,值用什么不重要,有污点就行
kubectl taint node <platform-node> kubezoo.io/pool=platform:NoSchedule
```

⭐ 这一条**不写任何代码**,却把故障模式从"**静默落到平台节点上**"变成"**停住不动**" ——
后者可见、可告警、不造成任何越界。

⚠️ 代价:平台自己的系统组件必须显式容忍这个污点(DaemonSet 通常已经容忍
`node-role.kubernetes.io/control-plane`,但**不会**自动容忍你新加的这个)。
上线前先确认 kube-system 里该跑的都还在跑。

⚠️ 而这条**只是兜底,不是隔离**:Pending 意味着"没落错",不意味着"落对了"。
真正的隔离仍然是注入的 nodeSelector —— 见 `docs/pod-spec-audit-cn.md` §2① 的结论:
那条路径今天**只有 Kyverno 一层**。

### ⚠️ 排空节点时:租户的 PodDisruptionBudget 会卡住你,但锁不死你

租户可以自由创建 PDB(kubezoo、控制器、策略层**都不管它**),包括
`minAvailable: 100%` / `maxUnavailable: 0` —— 那样它的 Pod **永远不接受驱逐**。

⭐ **但 PDB 只约束驱逐 API**(`pkg/registry/core/pod/storage/eviction.go`),
处理普通 DELETE 的 `strategy.go` 里零引用 ⇒ **直接删除完全绕过它**。

| 你用什么排空 | 结果 |
|---|---|
| `kubectl drain`(默认走 eviction) | ⛔ 卡住,反复 429 |
| `kubectl drain --disable-eviction` | ✅ 直接删,PDB 无效 |
| cluster-autoscaler 缩容 | ⛔ 尊重 PDB,该节点永远缩不掉 |

⚠️ **没有加代码守卫,是有意的**:拒绝 `minAvailable: 100%` 会打断租户正当的高可用配置,
而平台本来就有更大的锤子。代价是**租户会失去优雅停机** —— 强删不等它的 preStop 跑完。
所以真要强删前,先看一眼是不是某个租户把自己钉死了,能沟通就沟通。

⚠️ 落点隔离生效后,租户 Pod 都在**自己的节点池**上,所以它卡住的是自己那批节点。
但平台做全集群内核升级时,那批节点照样会挡路。

---

## 2.5 ⭐ 每租户 namespace 数量上限

`--max-namespaces-per-tenant=<N>`,**默认 0 = 不限**(升级不会打断任何人)。

⚠️ 这是**放大器的天花板**,不是计费控制。租户的跨 namespace list 是**逐个 namespace
读过去**的(`listAcrossNamespaces`,一个 namespace 一次上游请求,串行),所以租户每跑一次
`kubectl get pods`,就要花掉**与它拥有的 namespace 数一样多的上游请求** —— 打的是所有租户
共用的那台 apiserver。**大多是空的 namespace 是最坏情况**:走查必须把它们全趟一遍才能凑满
一页。

```bash
# 设之前先数:每个租户现在有多少
kubectl get ns -L kubezoo.io/tenant --no-headers | awk '{print $NF}' | sort | uniq -c | sort -rn
```

⛔ **设成低于某个租户的存量,它下一次建 namespace 就会被拒。** 已有的不受影响,
**而且必须不受影响** —— 一个超限的租户仍然要能写自己已有的 namespace,否则它连"删掉里面的
负载好回到限额以内"这件唯一该做的事都做不了。

⭐ 控制器自己会给每个租户建 4 个(`default` / `kube-system` / `kube-public` /
`kube-node-lease`),它们也算在内。所以 N 要留出这 4 个的余量。

⚠️ 计数失败时**放行**,不拒绝:上游抖一下不该让租户看成配额 —— 它分不清这两者。
日志里会有一条 `counting tenant ... to apply --max-namespaces-per-tenant`。

### 另外两个同类上限

同一个想法的另外两维,**都默认 0 = 不限**,失败模式和上面一致(只拒新建、计数失败放行):

| 参数 | 封的是什么放大 |
|---|---|
| `--max-cluster-role-bindings-per-tenant` | 租户的**一条** CRB 会投影成**每个租户 namespace 一条** RoleBinding ⇒ 成本是 **namespace 数 × CRB 数** |
| `--max-crds-per-tenant` | 每个租户 CRD 都是上游一个**真** CRD ⇒ 进上游 discovery 与 OpenAPI,**该集群的每个客户端都要下载**;kubezoo 侧还有一个 informer 缓存全部 CRD + 逐个类型转换器 |

⭐ **CRD 那条与前两者有个本质区别**:namespace 和 CRB 的代价是**每次请求**付的,CRD 的代价是
**常驻**的 —— 租户建完再也不用,别的租户照样天天付。所以它是三者里最该设的。

```bash
# 每个租户现有多少 CRD(按 group 前缀,不是按名字)
kubectl get crd -o custom-columns='GROUP:.spec.group' --no-headers \
  | grep -oE '^[0-9]{6}-' | sort | uniq -c | sort -rn
```

⚠️ 三个上限都**只拒新建**,存量一律照常可写可删。这不是宽容:**删掉一个是超限租户回到
限额内唯一的路**,拒掉它的删除等于把上限做成了陷阱。

---

## 2.6 ⭐ 租户凭据:发一次,不留底

租户的 kubeconfig 由控制器生成,**base64 放在 Tenant 对象的 annotation 上**:

```bash
kubectl get tenant <租户ID> \
  -o jsonpath='{.metadata.annotations.kubezoo\.io/tenant\.kubeconfig\.base64}' | base64 -d
```

⛔ **那份 kubeconfig 里含租户的私钥**,所以平台**不会一直留着它**。
默认签发 **24 小时**后控制器丢弃这份副本(`--credential-retention`),
只保留一条签发时间标记 `kubezoo.io/tenant.credential-issued-at`。

**理由不是洁癖**:一个集群级对象上放着**每一个租户的私钥**,读一次等于全部;
而留着它对平台**没有任何用处** —— 客户端证书**没有吊销机制**,这份副本不构成任何筹码,
只是暴露面。租户也从来没要求平台永久保管自己的私钥。

### 再要一张

**删掉签发标记即可**,下一次调谐就会签发新的并重新写进 annotation:

```bash
kubectl annotate tenant <租户ID> kubezoo.io/tenant.credential-issued-at-
```

⭐ 这样一来**轮换节奏回到了要凭据的人手里** —— 平台不替租户决定什么时候换。

⚠️⚠️ **撤回不是吊销,两件事。** 上面丢弃的只是**平台手里的副本**;
租户已经拿走的那张证书**照常有效直到过期**。
要让一个已经持证的租户立刻失效,用 `spec.suspension`(见 §3),那是另一套机制。

### 有效期:租户在平台上限内自选

| 参数 | 位置 | 默认 | 含义 |
|---|---|---|---|
| `spec.credentialValidity` | Tenant 对象 | 不填 | **租户自己选**。不填用平台默认 |
| `--tenant-credential-validity` | 控制器 | **90 天** | 租户不选时给多少 |
| `--max-tenant-credential-validity` | 控制器 | 365 天 | 租户最多能要多久。超了**截断并记日志**,不拒绝 |

⭐ **为什么拆成两个决定**:多久回来换一次凭据,取决于租户自己的 CI 怎么接、多少人手里有
kubeconfig、它自己的合规要求 —— **平台答不了**。而平台愿意在多长时间内"没法切断这个租户",
**租户也答不了**。所以租户选、平台封顶。

⚠️ 超上限**截断而不是拒绝**:拒绝会让租户在一个**它有权设置的字段**上把自己搞成没有可用凭据,
而错误只出现在控制器日志里、它根本看不到。

⭐⭐ **这就是"单租户吊销"的实际答案**。客户端证书没有吊销机制,所以有效期是
"平台不想让它继续用之后,它还能用多久"的**唯一上界**。配上"发一次不留底 + 删标记再要一张"
这套续签回路,**"切不断单个租户"就变成了"切断单个租户需要等最多一个有效期"**。
真要即时切断,用 `spec.suspension`。

⚠️⚠️ **默认值是 90 天,不是以前的 10 年** —— 这是行为变更,**只影响新签发的凭据**,
已经签出去的不受影响。到期时间记在
`kubezoo.io/tenant.credential-expires-at`(签发时写入,**副本撤回后仍保留**,
因为那时平台已经无从知道租户手里是哪一张):

```bash
kubectl get tenant -o custom-columns=\
'TENANT:.metadata.name,EXPIRES:.metadata.annotations.kubezoo\.io/tenant\.credential-expires-at'
```

⚠️ **把这条纳入巡检**。10 年的到期没人需要盯,90 天的需要 —— 否则第一次得知凭据过期,
是某天早上 CI 的一个 401。

⚠️ 三条运维须知:

- **升级不会重签**:本功能之前建的租户有 kubeconfig 但没有标记,控制器**认领**它
  (只补标记、不换证书),保留期从升级那一刻开始算
- **`--credential-retention=0` 关闭撤回**,给那些"晚于任何窗口才来取凭据"的开通流程
- **对 Pod 零影响**:租户负载用的是投影 SA token,不是这张证书;
  影响面只有拿 kubeconfig 的人和 CI

---

## 3. 日常操作:停机一个租户

### 3.1 两种模式,先选对

| | `ReadOnly` | `Frozen` |
|---|---|---|
| 场景 | 欠费 | 调查 / 争议 |
| 租户 `kubectl` | 能看,不能改 | **什么都不接受** |
| 租户的 Pod | 照常跑 | 照常跑 |
| 租户 Pod 的 SA 写上游 | 不拦 | **拦**(VAP) |
| 会不会删东西 | 不会 | **不会** |

两种模式**都不会停掉租户的工作负载**。这是有意的:欠费和调查都希望负载原样保留。

### 3.2 停机

```bash
# 欠费
kubectl patch tenant <租户ID> --type=merge \
  -p '{"spec":{"suspension":{"mode":"ReadOnly","reason":"invoice 2026-07 overdue"}}}'

# 调查
kubectl patch tenant <租户ID> --type=merge \
  -p '{"spec":{"suspension":{"mode":"Frozen","reason":"ticket SEC-1234"}}}'
```

`reason` 会**原样出现在租户看到的报错里**,写清楚,别写内部黑话。

### 3.3 停机后必须验证(三条,缺一不可)

生效不是瞬时的(控制器要同步,约 15 秒)。

```bash
TID=111111

# ① 前门拒了吗
#    应看到:tenant 111111 is suspended and is frozen: ...
kubectl --kubeconfig <租户的kubeconfig> get pods

# ② 租户的每个 namespace 都打上冻结标签了吗(仅 Frozen)
#    两个数字必须相等
kubectl get ns -l kubezoo.io/frozen --no-headers | wc -l
kubectl get ns -l kubezoo.io/tenant=$TID --no-headers | wc -l

# ③ 上游那条 VAP 在不在
kubectl get validatingadmissionpolicy tenant-frozen-deny-writes
```

### 3.4 ⛔ 只做 §3.3 是不够的 —— 需要确信时做功能检查

**①②③ 全绿,冻结仍然可能是半失效的。** 实测演示:把 VAP 删掉之后 ——

```
ns 标签:4 个          ← 正常
前门:  Forbidden      ← 正常
但租户 SA 试写:configmap/probe-check created (server dry run)   ← 洞开着
```

要确信,必须**用一个有权限的租户身份实际试写一次**:

```bash
NS=111111-default

# 临时造一个有权限的探针身份(平台身份不受冻结影响,建得出来)
kubectl -n $NS create sa canary
kubectl -n $NS create rolebinding canary --clusterrole=admin \
  --serviceaccount=$NS:canary

# 服务端 dry-run 试写 —— 不会真的创建任何东西
kubectl -n $NS create cm probe-check --from-literal=a=b --dry-run=server \
  --as=system:serviceaccount:$NS:canary

# 期望:ValidatingAdmissionPolicy 'tenant-frozen-deny-writes' ... denied request
# 若看到 "created (server dry run)" ⇒ 冻结没生效,查 §5

# ⚠️ 用完立刻删掉,别留着
kubectl -n $NS delete rolebinding canary && kubectl -n $NS delete sa canary
```

⚠️⚠️ **不要拿一个没有权限的 SA 去试**(比如 `default`)。
它冻结前后都返回 `forbidden` —— 因为 RBAC 在 VAP 之前就短路了。
**看着像验过了,其实什么都没验**:VAP 根本没装也是这个输出。
**探针身份必须先有写权限**,否则这条检查是自欺。

### 3.5 解除

```bash
kubectl patch tenant <租户ID> --type=json -p '[{"op":"remove","path":"/spec/suspension"}]'
```

验证(约 15 秒后,两条都要):

```bash
kubectl get ns -l kubezoo.io/frozen --no-headers | wc -l     # 应为 0
kubectl --kubeconfig <租户的kubeconfig> get pods              # 应恢复正常
```

**两层必须一致。** 只有一层恢复 = 租户处于"半解冻",症状会很难查。

---

## 4. ⛔ 冻结做不到什么(别拿它当取证冻结用)

`Frozen` 冻的是**租户改动控制面的能力**,不是租户。它**不能**:

- **停掉容器里已经在跑的代码。** 租户完全可以预埋一个"失联即销毁"的进程,
  它不需要访问 API 就能干活
- **提供时间点一致的快照。** 冻结不保证"哪个 revision 的视图",给不了取证要的那种保证
- **触及节点层。** 真要硬冻,是节点级带外操作(如 `cgroup freezer` 暂停 Pod 内所有进程的
  内核调度 —— 不触发进程退出、完整保留内存上下文),那**不是 kubezoo 的事**,
  kubezoo 只负责把控制面冻住,让编排层有得可操作

⇒ **取证场景里,`Frozen` 是第一步,不是全部。**

---

## 5. 排障:五个"看着正常,其实没生效"

**每一条都在本项目的实测中真实发生过**,共同点是:**没有任何报错指向策略**。

### 5.1 策略 `READY=True` 但什么都不做

`Ready` 只说明策略语法有效、被接受了,**不说明它会拒任何东西**。
本项目在这上面栽过四次,其中一次连 webhook 都注册上了、日志里却没有那次请求。

> **判据:让它拒一个你确信该拒的东西。** `Ready` 不是判据。

### 5.2 改标签验策略,却改成了同一个值

`kubectl label --overwrite` 设成**和现在一样的值**时,patch 是空的,
**apiserver 直接短路,准入根本不会被调用**。看起来就像策略没生效。

> **验策略时,标签一定要改成一个不同的值。**

### 5.3 租户的 Deployment 永远不出 Pod,且没有任何报错

多半是某条策略**拦过头,把 kube-controller-manager 一起拦了**。
`tenant-frozen-deny-writes` 的表达式是"**放行不属于本租户的身份**",
写成"拒绝一切写"就会这样。

> **验证:在冻结的 namespace 里以平台身份建一个 Deployment,
> 看它能不能收敛出 ReplicaSet 和 Pod。**

### 5.4 平台自己也在租户 namespace 里建不了特权 Pod

策略是按 **namespace 的 `kubezoo.io/tenant` 标签**匹配的,**不看是谁提交的**。
所以运营想在租户 namespace 里跑一个特权排障 Pod,**也会被拒** —— 这是对的,
不是 bug(否则策略就成了"看提交者脸色",而提交者是可以伪装的)。

排障容器请放在**平台自己的 namespace** 里,用 `hostNetwork` / `nsenter` 那套从节点侧看;
或者临时给该 namespace 加一条例外策略,**用完立刻删**。

### 5.5 多条策略同时生效时,别把拒绝归错

一个对象可能同时违反好几条规则,**先拒的不一定是你在测的那条**。
判据是报错里的 `策略名: 规则名`,并且要把被测对象改成**只剩下被测的那一处违规**。

---

## 6. 上线检查单

- [ ] `kubectl get clusterpolicy` **7 条**全 `READY=True`
- [ ] `kubectl get validatingadmissionpolicy` **3 条**(这几条 `get clusterpolicy` 看不到)
- [ ] 做过一次存量修正:namespace 的 PSA 标签 **+ 存量违规 Pod 已删**
- [ ] 每个租户节点池的 `kubezoo.io/pool` 标签**互不相同**(§2,承重前提)
- [ ] **平台/系统节点也带 NoSchedule 污点**(§2):否则 Kyverno 一挂,
      无容忍的租户 Pod 会径直落到平台节点上
- [ ] 在 canary 租户上完整走过一遍冻结:§3.3 三条 + §3.4 功能检查 + §3.5 解除
- [ ] 冻结的 namespace 里,平台建的 Deployment 仍能收敛出 Pod(§5.3)
- [ ] 运营知道 `Frozen` **不是取证冻结**(§4)

---

## 相关文档

- `security-admission.md` —— 安全边界:kubezoo 管什么、不管什么
- `isolation-audit-cn.md` —— 每一条结论背后的实测记录与负向对照
- `kaaas-platform-architecture-cn.md` §8 —— 职责划分判据、策略层选型
- **kubezoo-contract** 的 `config/policy/README.md` —— 改策略前必读的那几个坑
