# 运营手册:kubezoo + 策略层怎么配合使用

> 这份文档给**运营 / SRE**。工程侧的推导和实测记录在
> `isolation-audit-cn.md` 与 `kaaas-platform-architecture-cn.md`,这里只讲**怎么做、怎么验**。

## 0. 一句话:kubezoo 单独装上,隔离是不完整的

kubezoo 是租户的**前门**,它只约束租户本人的 `kubectl`。
租户的 **Pod 拿 ServiceAccount token 直连上游 apiserver,完全不经过 kubezoo**
(实测:审计 §S)。所以下面这套策略**不是可选加固,是隔离的一半**。

**没装策略层的集群,租户可以**(每条都实测过):
跑出 kata 沙箱、拿全集群最高优先级抢占、建 privileged + hostNetwork 的 Pod、
往每个节点投放 DaemonSet、把 Pod 钉到任意节点、把 Pod 绑到别的租户的节点上。

---

## 1. 部署:必须装什么

```bash
kubectl apply -f config/policy/
```

装完确认**两类都在**:

```bash
kubectl get clusterpolicy                    # Kyverno 的,应有 5 条,READY=True
kubectl get validatingadmissionpolicy        # 原生的,应有 2 条
```

⚠️⚠️ **`config/policy/` 里有两条是 Kubernetes 原生的 `ValidatingAdmissionPolicy`,
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
| `tenant-frozen-deny-writes.yaml` | **原生 VAP** | **冻结只冻住租户的 kubectl**,它的 Pod 照常读写 |

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

- [ ] `kubectl get clusterpolicy` 5 条全 `READY=True`
- [ ] `kubectl get validatingadmissionpolicy` **2 条**(这两条 `get clusterpolicy` 看不到)
- [ ] 做过一次存量修正:namespace 的 PSA 标签 **+ 存量违规 Pod 已删**
- [ ] 每个租户节点池的 `kubezoo.io/pool` 标签**互不相同**(§2,承重前提)
- [ ] 在 canary 租户上完整走过一遍冻结:§3.3 三条 + §3.4 功能检查 + §3.5 解除
- [ ] 冻结的 namespace 里,平台建的 Deployment 仍能收敛出 Pod(§5.3)
- [ ] 运营知道 `Frozen` **不是取证冻结**(§4)

---

## 相关文档

- `security-admission.md` —— 安全边界:kubezoo 管什么、不管什么
- `isolation-audit-cn.md` —— 每一条结论背后的实测记录与负向对照
- `kaaas-platform-architecture-cn.md` §8 —— 职责划分判据、策略层选型
- `../config/policy/README.md` —— 改策略前必读的那几个坑
