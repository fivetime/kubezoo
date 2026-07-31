# 设计:租户 `-A` 的按 namespace 扇出

> 现状:`kubectl get pods -A` 走的是**一次全集群 LIST + 客户端侧过滤**
> (`pkg/proxy/proxy.go` 的 `list` → `util.FilterUnstructuredList`)。
> 正确,但**每个租户的每次 `-A` 都是一次全集群 LIST** —— 按北极星(规模优先)不能留。

## 0. 先纠正一个曾经写错的判断

本文档之前的版本(以及 `TODO-kaaas.md` 里挂了很久的那条)说:
**"扇出必须在分页 / `resourceVersion` 语义上做让步,需要产品拍板"**。

**这是错的。** 原生 apiserver 早就用同一个机制解掉了这个问题,扇出直接可以照抄。

### 原生分页为什么不怕"途中新增 namespace"

`continue` token 里带着 **revision**:

```go
// staging/src/k8s.io/apiserver/pkg/storage/continue.go
type continueToken struct {
	APIVersion      string `json:"v"`
	ResourceVersion int64  `json:"rv"`     // ← 快照点
	StartKey        string `json:"start"`
}
```

而且 store 强制每一页都来自同一个 revision:

```go
// staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go
} else if *chunk.revision != withRev {
	return fmt.Errorf("etcd returned revision %d for a list read at revision %d", ...)
}
```

⇒ **一次分页 LIST 就是一个快照。** 分页途中新建的对象和 namespace **根本看不见**;
快照被 compact 掉就 `410 Gone`,客户端重新开始。这是设计保证,不是巧合。

⇒ **扇出照抄即可**:把所有子 LIST 钉在同一个 revision 上。**没有语义让步。**

## 1. 方案

只改**没有指定 namespace**的那条路径(`-A`)。指定了 namespace 的请求一个字都不动。

```
list(ctx, options):
  若请求带 namespace  → 保持现状(一次带 scope 的 LIST)
  否则(-A):
    1. 解出我们自己的 continue token → {R, nsIndex, innerContinue}
       没有 token(第一页)→ R 未定
    2. 取该租户的 namespace 列表(按名字排序,顺序必须稳定):
         R 未定 → LIST namespaces?labelSelector=kubezoo.io/tenant=<tid>
                  取回来的 list 的 resourceVersion 就是 R
         R 已定 → 同样的 LIST,但带 resourceVersion=R & resourceVersionMatch=Exact
    3. 从 nsIndex 开始逐个 namespace:
         LIST <资源> in ns,带 resourceVersion=R, resourceVersionMatch=Exact,
                              limit=剩余额度, continue=innerContinue
         累积结果直到攒够 limit
    4. 还有剩 → 编码 continue token {R, nsIndex, innerContinue}
    5. 返回 list 的 resourceVersion 置为 R
```

### 为什么这样就等价于原生

| | 原生 | 扇出 |
|---|---|---|
| 快照点 | 一个 etcd revision | **同一个** revision R,钉在每个子 LIST 上 |
| 途中新建的对象/namespace | 看不见 | **看不见**(R 之后的都不在快照里) |
| 游标 | `{rv, startKey}` | `{R, 第几个 ns, ns 内 startKey}` —— **同一形状,多一个坐标** |
| 快照过期 | `410 Gone` | **`410 Gone`**,原样透传 |
| LIST 的 rv 能否引导 watch | 能 | **能**(就是 R) |

⚠️ **namespace 列表本身也必须钉在 R 上**,否则第 2 页可能看到第 1 页时还不存在的 namespace ——
那才是真正会出问题的地方,而不是"新建 namespace"本身。

## 2. 成本:比现状更小,不是更大

查过两处上游实现:

- **watch cache 按 namespace 建了索引**(`cacher.go` 用请求的 namespace 作 scope,
  或从字段选择器的 `metadata.namespace` 精确匹配取)⇒ 每个子 LIST 是**带 scope 的查找,不是全量扫描**
- **`ListFromCacheSnapshot` 自 1.34 起 Beta 且默认开** ⇒ `resourceVersionMatch=Exact`
  **可以由缓存快照服务**,不必打到 etcd

⇒ 扇出是 **N 次带 scope 的缓存读**(N = 该租户的 namespace 数),
现状是 **一次全集群扫描**。开销从 O(集群) 变成 O(租户)。

## 3. 真正需要定的东西(只有这些)

| | 取舍 |
|---|---|
| **namespace 数量上限** | 游标长度与 N 成正比。建议给每租户的 namespace 数设配额,并在扇出侧设一个硬上限,超过就报错而不是悄悄截断 |
| **R 的存活窗口** | 与原生同一种失效(compact → 410),但扇出 N 次往返耗时更长,**暴露窗口更宽**。这是程度差异,不是语义差异 |
| **游标编码** | `{v, rv, nsIndex, inner}` 的 JSON + base64,与原生同构。**必须自己的 token 与上游 token 不混淆** —— 收到不认识的 token 一律拒,不要当成上游的透传 |

## 3.1 ⚠️ 实现时撞到的三件事(都实测过)

**① `continue` 与 `resourceVersionMatch` 互斥。** apiserver 直接拒:

```
The ListOptions "" is invalid: resourceVersionMatch: Forbidden:
resourceVersionMatch is forbidden when continue is provided
```

因为 continue token 本身就带着 revision。所以规则是:
**新开一个 namespace 时钉 `resourceVersion=R + Exact`;续读某个 namespace 时只传 token**
—— 两种情况钉的是同一个快照,保证不变。

**② 游标坏掉不是"少返回",是"翻不完"。** 故意把游标里的 `Inner` 丢掉之后,
每次续读都从该 namespace 头部重来,**客户端永远翻不完**。
⭐ 所以守卫必须**带超时**:一个会挂住的检查和一个会绿的检查一样糟。
实测:`chunk-size=1/2/5` 三档全部 `did not finish inside 90s`,而不是报数字不符。

**③ 自己的 token 必须与上游的区分开。** 收到不认识的 token 一律拒
(`the continue parameter is not one this server issued`),**不要当成上游 token 透传** ——
上游 token 的含义是"全集群单一区间里的位置",透传过去会**静默列错东西**。

## 4. 分两步做

**第一步(已完成):LIST 扇出。** `pkg/proxy/fanout.go`。
守卫在 `hack/lab/verify.sh`(跨 namespace 计数 / 各 chunk-size 分页一致 / 结果里无前缀残留),
**已验证弄坏游标会红**(三档分页全部超时失败,而不是悄悄少返回)。

**第二步(已完成):WATCH 多路复用。** `pkg/proxy/watchmux.go`。
`-A` 的 watch 扇成 N 条流、对客户端合成一条,每条都从客户端给的 rv(即第一步的 R)起步,
所以 informer 的契约(LIST 拿到 rv,再从该 rv WATCH)是兑现的。
⭐ 这是**租户自装 operator 的必需件**:cluster-wide informer 走的就是这条路。

### ⚠️ watch 侧撞到的两件事

**① watch 期间新建的 namespace 必须动态加入,否则是"静默漏"。**
不加的话,租户在 watch 开着时建的 namespace 里的对象**永远不出现**,
而 informer 会一直以为自己是最新的 —— **缓存错了却什么都不说**,是最坏的一类失败。
所以 mux 另开一条 namespace watch,见到 `ADDED` 就给它加一条流。

**② 新 namespace 的第一次 watch 必然被拒,要重试。** 实测:

```
configmaps is forbidden: User "111111-admin" cannot watch resource "configmaps"
in the namespace "111111-wc"
```

namespace 事件比授权先到 —— RoleBinding 由控制器写(~170ms),再要到达授权器缓存(~310ms)。
所以加入动作走独立 goroutine + 有界重试(只对 Forbidden 重试),
重试用尽仍失败**必须记日志说清楚**:否则客户端会保有一条看着健康、却永远不提那个 namespace 的流。

**③ ⭐ 一条流断掉必须让整条合流断掉,而且要带 410。** 这条改过两次才对。

第一版:某个 namespace 的上游流结束后,它永远留在 `watched` map 里,没有任何东西会重开它。
第二版加了 `forget()` 把它从 map 里删掉 —— **必要但不充分**,而且当时那条注释写错了:
唯一会重开一个 namespace 的路径是 namespace follower 上的 `ADDED` 事件,
**而一个从未被删除的 namespace 永远不会再产生 `ADDED`**。
所以流结束的四种原因里,只有"删掉又用同名建回来"真的修好了;
apiserver 自己的随机 watch 超时、上游滚动重启、瞬时断连这三种,**行为与修复前逐字节相同**。

只重连也不够,原因更隐蔽:informer 记住的 rv 是它最后收到的那个,
而**一条流的 rv 说明不了另一条流的进度**,从它续订会跳过慢流还没交付的事件。
`expire()` 发 410 再结束是唯一没有这个缝的做法:任何客户端收到 410 都会重新 LIST
(这正是它处理 compaction 的方式),扇出从一个一致的快照重新开始。

同理:**namespace follower 自己结束也必须 410**。它是唯一会加入新 namespace 的 goroutine,
它一退休,此后新建的 namespace 全部不可见 —— 连"join 失败"的警告都不会打,因为根本没尝试过 join。

**④ 上游各条流不继承客户端的超时。** 否则它们会和客户端自己的请求同时到期、其中一条抢先几毫秒,
而"流结束 = 410" ⇒ 每个 reflector 五到十分钟的正常周期都变成一次全租户重列。
不设时,上游用自己的 `minRequestTimeout`(下限半小时),这些流活得比开启它们的请求久,
由 `Stop()` 一起拆掉。这样 410 才真的意味着"出事了"。

**⑤ bookmark 换成跨流最小值。** cacher 给每条流的 bookmark 盖的是**集群当前 rv**,不是该 namespace 的,
所以原样转发会把客户端的续订点推过另一条流还没交付的事件 —— 而且这条流可以毫无活动。
`onBookmark` 取各流"已交付到"的最小值再发一条,这是客户端唯一能安全续订的 rv。
WatchList 的 `initial-events-end` 同样扣住,直到每条流都回放完为止,
否则客户端会拿着"只装了一个 namespace"的缓存被告知同步完成。

**残留(明说而不粉饰)**:合流是按流有序、不是按 rv 有序,所以在两次 merged bookmark 之间,
客户端记住的 rv 可能高于另一条流还没交付的事件。若客户端恰在这个窗口里自己结束请求
(它自己的超时,这个文件看不见)并从该 rv 续订,那条事件就漏了。
bookmark 是用来兜底的:每约一分钟把续订点拉回安全值。
彻底关掉需要"每条事件都等所有其它流报告过才交付",代价是每条事件都要付一个 bookmark 间隔的延迟,不值。

守卫:`verify.sh` 两条(已有 namespace 的事件到达 / watch 期间新建的 namespace 能加入),
**已验证关掉 mux 会红**(恰好这两条,其余 28 条不受影响);
③⑤ 由 `pkg/proxy/watchmux_test.go` 三条单测把住(一条流结束→客户端收到 410 且合流关闭 /
bookmark 是最小值而非 5000 / Stop 之后 expire 不 panic)。

## 5. 与其它方案的对比(为什么不选)

| | 完整性 | 分页 / rv | apiserver 侧成本 |
|---|---|---|---|
| 现状:全量 LIST + 过滤 | ✅ | ✅ | ⛔ O(全集群) |
| 标签选择器 `kubezoo.io/tenant=<tid>` | ⛔ **会漏** | ✅ | ⛔ O(全集群) |
| **按 namespace 扇出(本方案)** | ✅ | ✅ | ✅ O(租户) |
| 直接读存储做前缀 range | ✅ | ✅ | ✅ | ⛔ 等于重写 apiserver 读路径 |

**标签方案为什么会漏**:标签得由 kubezoo 在写入时强制覆盖,但**上游控制器代租户创建的对象
不经过 kubezoo** —— Deployment 生出的 Pod 是 kube-controller-manager 直接建的。
Pod 模板可以由 kubezoo 盖章后被继承,但 EndpointSlice、StatefulSet 的 PVC 这类
由控制器凭空建的对象没有这条路。**"少返回"比"多返回"危险得多。**

**namespace 是唯一永远正确的判据**:不管谁创建,租户的对象一定在租户的 namespace 里。

**API 层为什么做不了前缀**:字段选择器只有 `=` / `==` / `!=`
(`fields.Requirement` 就一个 `Value` 加一个小操作符集),没有前缀也没有通配。

## 6. ⛔ cluster 级资源:试过标签方案,**实测危险,已撤回**

集群级资源没有 namespace 可扇,API 上唯一能用的判据是 **label selector**。
看起来该做,而且有一条很有说服力的不对称:

> namespaced 的对象里,上游控制器代租户建的那些(Deployment 生的 Pod)不经过 kubezoo,
> 拿不到标签 ⇒ 标签会漏。
> 但**集群级对象租户能看见的本来就只有名字带前缀的那些**,全部由 kubezoo 经手创建 ⇒
> 标签的完整性和现有前缀判据**完全等价**。

实现过一版:写入口盖 `kubezoo.io/tenant` 标签,列举时把它作为 label selector 下推,
**并保留前缀检查做权威**(这样标签错了只会少返回自己的东西,永远不会跨租户泄漏)。

### 实测结果:所有**存量**对象从租户视角消失

新建的对象有标签,升级前建的没有。开启下推之后:

```
租户 111111  PV: 只剩新建的 pv-c(旧的 pv-a 不见了)
租户 222222  PV: 空        CRD: 空      ← 这个租户的东西全没了
撤回之后     两个租户的新旧对象全部恢复
```

⇒ 放到生产就是:**升级 kubezoo → 每个租户的 PV / CRD / ClusterRole 全部"不存在"**
→ 他们的 `kubectl apply` 会重建、helm 看到漂移、**`--prune` 会真的删掉东西**。

### 为什么不靠"升级时补跑一次 backfill"糊过去

- 从换二进制到跑完 backfill 之间,**所有租户是瞎的**
- 运维忘了这一步 ⇒ **静默丢失**,没有任何报错指向它
- 而且没法自证:label selector 返回空,**分不清"确实没有"和"没打标签"**

### 而且收益本来就小

集群级对象的量是 **O(租户数 × 每租户几十个)**,不是 O(工作负载)。
和 namespaced 那条(O(全集群 Pod))差着数量级。
标签也**不降低 apiserver 的扫描成本**(标签没有索引),只省网络传输和 kubezoo 侧内存。

⇒ **决定:不做。** 现状(全量 LIST + 前缀过滤)保留。
⚠️ 若将来真要做,前提是**先有一个能自证完整的 backfill**,而不是一个运维步骤。

> 这条也是给自己的教训:我整场都在说"**少返回比多返回危险得多**",
> 然后自己写了一版会让存量对象整片消失的改动。
> **是实测把它拦下来的,不是推理。**
