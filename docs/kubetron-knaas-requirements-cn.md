# kubetron 在 KNaaS 形态下的需求（kubezoo 侧提出）

> 状态：需求，**不含 kubetron 代码改动**。行号基于 kubetron `main`（2026-08 检视）。
> 提出方：kubezoo（KNaaS 共享控制面）。对应架构定案 B1，见 `kaaas-platform-architecture-cn.md`。

## 1. 背景：一个 kubetron 没有被设计去面对的部署形态

kubetron 现在的契约是（README §「Operator 每 namespace 的输入」）：

> Operator 每 namespace 的输入是一个 ConfigMap（`kubetron-network`）：`network`、`credentialSecretRef`、`serviceSubnetID`、可选 `dnsNameservers`。两个 ID 一个 Secret 名——拓扑决策全部不在 kubetron。

这个契约隐含一个前提：**namespace 是运维划的,运维能写而使用者不能写**。在「namespace = 团队、集群管理员就是 operator」的独立部署里,这个前提成立,现在的设计没有问题。

在 KNaaS（kubezoo）下这个前提不成立：

- 租户拿到的是 namespace,**租户在自己 namespace 里是 `*` on `*`**（这是 KNaaS 的定义,不是配置疏漏）；
- 于是「operator 提供的输入」和「租户可写的对象」**落在同一个位置**；
- 结果：**平台拓扑决策变成了租户可写的输入**。

下游的 `authURL`、`credentialSecretRef`、`subnetID`、`shard` 全部继承这一处错位 —— 逐个字段去补是补不完的。

## 2. 现状证据

### 2.1 平台配置的读取点（4 处,全部从租户 namespace 读）

| 位置 | 读什么 | 用途 |
|---|---|---|
| `pkg/service/reconciler.go:234` | `kubetron-network` in `svc.Namespace` | `serviceSubnetID`、`credentialSecretRef` |
| `pkg/service/reconciler.go:516` | 同上（删除路径 `deleteCreds`） | 级联删除用的凭据 |
| `pkg/controller/sts_controller.go:76` | 同上 in `sts.Namespace` | `network`、`credentialSecretRef`、`dnsNameservers` |
| `pkg/webhook/claim_webhook.go:57`、`:83` | 同上 in `req.Namespace` | `shard` |

### 2.2 租户可写的输入路径（5 条）

| # | 路径 | 位置 | 平台影响 | 是否被别的机制兜住 |
|---|---|---|---|---|
| T1 | 直接改 `kubetron-network` ConfigMap 的任一键 | 上表 4 处 | 全部下游决策 | 否 |
| T2 | Service 注解 `kubetron.network.kubevirt.io/credential-secret` 覆盖凭据名 | `reconciler.go:239`、`:511` | 换掉本次调用用的凭据 | 否 |
| T3 | `NetworkPortClaim.Spec.CredentialSecretRef` | `claim_controller.go:364` | 同上 | 否 |
| T4 | 直接在 claim 上写 `kubetron.network.kubevirt.io/shard` 标签 | `claim_webhook.go:51` — `if claim.Labels[ShardLabel] != "" { return admission.Allowed(...) }` | 指定由哪个 kubetron 实例处理 | 否 |
| T5 | 凭据 Secret 里的 `authURL` | `reconciler.go:252`、`claim_controller.go:368` | 平台控制器向该 URL 发起连接 | 否 |

### 2.3 严重度（据实排序,不夸大）

- **高 — T5 `authURL`（SSRF）**：`reconciler.go:252` 与 `claim_controller.go:368` 把租户 Secret 里的 `authURL` 直接交给 `NewOctavia` / `TenantClient`,**全仓没有任何 `url.Parse` 校验、没有允许清单**。控制器跑在平台侧,能触达管理网。凭据本身是租户自己的（这是有意设计,见 `claim_controller.go:66-69` 注释:访问控制由 OpenStack project scope 承担）,所以**这不是凭据窃取,是平台控制器被当作跳板向任意地址发起出站连接**。
- **中 — T4/T1 `shard`**：租户能指定由哪个 kubetron 实例服务自己。分片是平台的爆炸半径/隔离决策,不该由被隔离方选。注释 `claim_webhook.go:22` 写的是「tenants normally never set it」—— 在 KNaaS 下 normally 不成立。
- **低（架构错位,但被 Neutron RBAC 兜住）— T1 的 `network`/`serviceSubnetID`、T2/T3 的凭据名**：租户把 `network` 改成别人的 VPC,用的仍是自己的应用凭据,Neutron 会拒。所以**不构成跨租户逃逸**,但让平台的拓扑决策变得不可信,也让报错归因困难。

## 3. 需求

### R1（根因）平台配置迁出租户 namespace

新增一个平台侧配置位置开关（示意:`--platform-config-namespace=kubetron-system`）。设定后：

- 上述 4 个读取点**只**从该 namespace 读,按**租户 namespace 名**索引（例如对象名即 `<tenant-namespace>`）；
- **不得回落到租户 namespace** —— 回落等于没改；
- 未设定时保持现状,独立部署形态零影响。

验收：在租户 namespace 里创建一个内容完全不同的 `kubetron-network`,平台行为**逐字节不变**；平台侧删掉对应配置后,请求以「无平台配置」失败,而不是悄悄采用租户那份。

### R2 `authURL` 不再来自租户可写来源

`authURL` 改由平台决定。两种形态都要支持：

- 单 endpoint：取平台侧配置（`OS_AUTH_URL` 已存在于 `cmd/manager/main.go:235,259,356`）；
- 多 region：平台侧维护 **名称 → URL** 的允许清单,租户/平台配置里只能出现**名称**,永远不出现 URL。

验收：一个负向对照 —— 凭据 Secret 里塞入 `authURL: http://169.254.169.254/...`,断言控制器**没有**向该地址发起连接,且报错指明 authURL 被忽略。（注:此项即使 R1 完成也仍需要,因为凭据 Secret 按设计是租户提供的。）

### R3 删除 T2 注解覆盖

`reconciler.go:239` 与 `:511` 的 `CredentialAnnotation` 覆盖删除。平台决策不接受租户可写字段作输入。若独立部署确有需要,改为**只在未设 R1 开关时**生效。

### R4 shard 由平台无条件决定

- `claim_webhook.go:51` 的「已设则放行」改为**无条件覆盖**为平台解析出的 shard；
- CREATE 之外,UPDATE 上对该标签的修改也要拒（否则先建后改即可绕过）。

验收：负向对照 —— 租户创建时带 `shard: other`,断言最终对象上是平台值；再 patch 一次,断言被拒。

### R5 报错不回泄平台配置

R1 之后,租户读不到平台配置,但控制器的报错会写进租户可见的 Event/Status。`reconciler.go:235`、`sts_controller.go:77` 这类信息里不应出现平台 namespace 名、subnet ID、Secret 名等内容。

## 4. 明确不在本需求内

- **租户提供自己的 OpenStack 应用凭据**：这是有意设计,保留。承重的是 OpenStack project scope 而不是 k8s namespace（`claim_controller.go:66-69`）。本需求只要求**endpoint 不由租户给**。
- **kubetron 感知 kubezoo 的租户 ID 前缀**：不需要。kubetron 只看 namespace 名,kubezoo 已经把租户 namespace 改写成 `<tid>-<ns>` 的形式,按 namespace 名索引平台配置天然按租户隔离。
- **kubetron 自己做多租户鉴权**：不需要,租户到不了 kubetron 的 API —— 所有请求经 kubezoo。

## 5. 对 kubezoo 侧的影响

R1 落地后,kubezoo 需要在租户开通时于平台 namespace 写入该租户的 kubetron 配置对象。这属于 kubezoo 租户控制器的职责,与现有的 ResourceQuota / 默认 RBAC 下发同一条路径,无新增机制。
