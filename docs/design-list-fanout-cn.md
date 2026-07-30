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

## 4. 分两步做

**第一步(本次):LIST 扇出。** 上面全部。可独立验证、可独立回滚。

**第二步(后续):WATCH 多路复用。** `-A` 的 watch 同样要扇出成 N 条流,
对客户端合成一条。它现在能**干净地从 R 起步** —— 这正是第一步给出的东西。
informer 的契约(LIST 拿到 rv,再从该 rv WATCH)因此可以兑现。
⭐ 这是**租户自装 operator 的必需件**:operator 的 cluster-wide informer 走的就是这条路。

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
