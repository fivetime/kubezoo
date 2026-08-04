# 给租户卷快照:定案与依据(#104)

> 状态:**决策已定,依据已查实**;实现进行中。
> 依据全部来自 external-snapshotter `c3a43e4` 与 k8s 1.36 源码实读,不是按同构推。

## 0. 先更正一条我写错的结论

曾写过"VolumeSnapshot **结构性**给不了:快照 CRD 会被加租户前缀,平台 snapshot-controller
永远看不见"。**那只在租户自己装 CRD 时成立。**

平台提供 Ceph ⇒ 快照 CRD 由**平台**装,组名就是 `snapshot.storage.k8s.io`,不走加前缀那条路。
真正的拦路点小得多:kubezoo 判定 CRD 归属**只看名字前缀**
(`util.ListCRDsForTenant` → `UpstreamObjectBelongsToTenant`),平台 CRD 不带前缀
⇒ 不属于任何租户 ⇒ 租户看不见。**缺的是功能,不是可能性。**

⚠️ 同时更正第二条:我把 `TODO-kaaas.md` §W 的 confused deputy 顾虑**整段搬来却没验**。
对主要字段它**不成立** —— 见 §2 第一行。

## 1. 定案

| 对象 | 作用域 | 给租户 | 依据 |
|---|---|---|---|
| `VolumeSnapshot` | 命名空间级 | ✅ **可读可写** | 租户从**自己的** PVC 打快照,这是它要的能力 |
| `VolumeSnapshotClass` | 集群级、平台所有 | ✅ **只读,第五个发布类** | 与 StorageClass 完全同构:决定用哪个驱动、哪种删除策略,是平台卖的档位 |
| `VolumeSnapshotContent` | 集群级 | ⛔ **完全不提供** | 见 §3,这是本设计里唯一的真实逃逸点 |
| `VolumeSnapshot.spec.source.volumeSnapshotContentName` | — | ⛔ **拒绝**(仅 CREATE) | 预置绑定,前提是租户先持有一个 content;既然 content 不给,这个入口也必须堵 |

## 2. 逐字段判定(VolumeSnapshot 是租户唯一能建的对象)

| 字段 | 处理 | 为什么 |
|---|---|---|
| `spec.source.persistentVolumeClaimName` | **不翻译** | 上游注释明写 "This PVC is assumed to be in the same namespace as the VolumeSnapshot object"。落到 `<租户ID>-default` 后自然解析到租户自己的 PVC ⇒ **该字段没有跨租户可达性**,§W 的 confused deputy 顾虑在这里不成立 |
| `spec.source.volumeSnapshotContentName` | ⛔ **CREATE 时拒绝** | 见 §3 |
| `spec.volumeSnapshotClassName` | **不加前缀**,未发布则拒 | 集群级平台对象,和 `storageClassName` 同构 |
| `status.boundVolumeSnapshotContentName` | ⭐ **原样返回,不得报错** | 控制器写的 `snapcontent-<uid>`,**直接建在上游、不带租户前缀** —— 与动态供给的 PV **完全同一形状**。`pkg/convert/pvc.go` 刚为此付过代价:一个无法归属的名字让 Backward 报错 ⇒ **整个 list 失败**,租户连删都删不掉 |

## 3. ⛔ 唯一的真实逃逸点:`snapshotHandle`

`VolumeSnapshotContent.spec.source.snapshotHandle` 是**存储系统上的真实快照句柄**
(Ceph 上就是 RBD/CephFS 的快照标识)。

租户若能建 content:

```yaml
kind: VolumeSnapshotContent
spec:
  source: {snapshotHandle: "<另一个租户的快照句柄>"}
  volumeSnapshotRef: {namespace: <自己的 ns>, name: mysnap}   # 指回自己,校验会过
```

控制器正常绑定 ⇒ 租户从中恢复出 PVC ⇒ **拿到别人的数据**。

⭐ **这和 PV 那条同源**:PV 那次是租户可控的 `nfs.server`,这次是 `snapshotHandle`。
⚠️ **而且 PV 那次的修法在这里不管用**:那次靠"必须写 `claimRef`"把卷预留给自己人,
但这里 `volumeSnapshotRef` 管的是**绑给谁**,`snapshotHandle` 才是**载荷** ——
指回自己完全合法,数据照样是别人的。

⇒ **预置快照(pre-provisioned)是数据导入原语,必须平台专有。** 租户只能做动态快照。

## 4. 查实过的:预置绑定的跨租户,上游自己堵住了

⭐ 这一条我原本担心是第二个 PV 式漏洞,**查下来不是**,值得记下来:

`checkandBindSnapshotContent`(`snapshot_controller.go:1107`)**只比 Name 和 UID,不比 Namespace** ——
单看它像个洞。但静态路径的**入口** `getPreprovisionedContentFromStore`(同文件 :642)先做了完整校验:

```go
if ref.Name != snapshot.Name || ref.Namespace != snapshot.Namespace ||
   (ref.UID != "" && ref.UID != snapshot.UID) {
    → "VolumeSnapshotContent is bound to a different snapshot"
}
```

⇒ 命名空间**有**被比。所以即便将来放开 content,绑定层面的跨租户是被上游挡住的
—— 但那**不改变 §3 的结论**,因为 §3 的问题不在绑定层面。

⚠️ **PV 的教训在这里是双向的**:不能假设安全,也不能假设不安全。我两次都差点推错。

## 5. 实现清单

- [ ] `apigroups.go` 声明 `snapshot.storage.k8s.io`:`volumesnapshots`(+`/status`)、
      `volumesnapshotclasses`(只读发布类);**`volumesnapshotcontents` 不声明**
- [ ] `pkg/convert/volumesnapshot.go`:按 §2 的表处理四个字段
- [ ] `pkg/proxy`:`refuseUnpublishedSnapshotClass`(CREATE)+ `refusePreProvisionedSnapshot`(CREATE)
      —— ⚠️ 三条写路径都要有,`TestEveryWritePathRunsTheSameGuards` 会查
- [ ] `publishedclass`:第五个 informer + `common.VolumeSnapshotClassPublishedLabelKey`
- [ ] contract:`ClusterScopedRules` 加 `volumesnapshotclasses` **只读**;
      ⚠️ `volumesnapshots` 是**命名空间级**,不要放进集群级表(刚犯过这个错,见 v0.27.1)
- [ ] lab:装 external-snapshotter(CRD + snapshot-controller),租户对自己的 PVC 打**真快照**、
      从快照**恢复出新 PVC 并读到原数据**;负向对照:建 content 被拒、预置快照被拒、未发布类被拒

## 6. 明确不做

- **通用"共享 CRD"机制**。§W 推迟它的理由是"平台 operator 用平台凭据调谐租户 spec ⇒ 必须针对
  具体 operator 审查"。本文就是那次审查,而它的产物是**一条针对 snapshot 的具名集成**
  (含 §3 这个只有这个 CRD 才有的判断),不是一个可以被下一个 CRD 复用的口子。
- **VolumeGroupSnapshot**。同族但更晚,等这条跑通再看。
