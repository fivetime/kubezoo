# kubezoo-gateway

租户直接访问的 API 服务端。它在同一个物理集群上翻译出多个租户视角:
租户看到的是自己的一个集群,而底下是同一套控制面和数据面。

叫 gateway 而不是 proxy,是因为它**不转发** —— 它终结租户侧的 API、
双向改写请求与响应、提供自己的资源、并过滤 discovery 对外宣告的内容。

⚠️ 与 [kubesluice](https://github.com/fivetime/kubesluice) 不是一回事 ——
那是另一个项目,而且可以**挡在这个前面**;它源自上游的
[kubegateway](https://github.com/kubewharf/kubegateway)。

[English](./README.md) | 简体中文

## ⚠️ 这是三个仓库之一

KubeZoo 原本是一个仓库,现在是三个,**部署时三个都要**:

| | |
|---|---|
| **kubezoo-gateway**(本仓库) | 租户直接访问的 API 服务端。终结租户侧 API,双向翻译成上游调用 |
| [kubezoo-contract](https://github.com/fivetime/kubezoo-contract) | 翻译规则、API 类型、准入策略。另外两个仓库都依赖它 |
| [kubezoo-controller](https://github.com/fivetime/kubezoo-controller) | 把上游集群对账成这里保存的 Tenant 声明的样子 |

⛔ **只装这一个的话,集群会接受 Tenant 对象然后什么都不做** —— 没有 namespace、
没有 RoleBinding,而且**没有任何报错指向缺了什么**。它看起来一切正常,
直到第一个租户真的去用。

为什么分开:apiserver 是**全活**的,控制器不是。合在一起时每个 kubezoo 副本都会跑
一份控制器,三副本就是三份同时对账同一批租户。拆开之后,"要几个代理"和"要几个控制器"
才成为两个可以分别回答的问题 —— 而两个答案不一样,控制器目前只能 1(原因见它的 README)。

## 简介

KubeZoo 是轻量级的 Kubernetes 多租户项目，基于协议转换的核心理念在一个物理的 K8S 控制面上虚拟多个控制面，具备轻量级、兼容原生 API 、无侵入等特点。
详细设计请参考 [设计文档](./docs/design-cn.md)


<div align="center">
  <!--[if IE]>
    <img src="docs/img/kubezoo-overview.png" width=80% title="KubeZoo Overview" loading="eager" />
  <![endif]-->
  <picture>
    <source srcset="docs/img/kubezoo-overview-dark.png" width=80% title="KubeZoo Overview" media="(prefers-color-scheme: dark)">
    <img src="docs/img/kubezoo-overview.png" width=80% title="KubeZoo Overview" loading="eager" />
  </picture>
</div>

## 为什么选择 KubeZoo

社区 Kubernetes Multi-Tenancy Working Group 定义 [3 种 Kubernetes 多租户模型](https://kubernetes.io/blog/2021/04/15/three-tenancy-models-for-kubernetes/)，例如： Namespace as a Service (NaaS)、Cluster as a Service (CaaS)、Control Planes as a service (CPaaS)，这些模型侧重于不同的场景。放眼公有云和部分私有云场景，通常会碰到如下问题：首先海量的小 K8S 集群更像是云上的常态；其次用户期望快速的交付一个 Kubernetes 环境；最后则是海量 Kubernetes 集群带来的巨大的运维管理成本。

增强 K8S 集群多租户功能，使其具备极低的资源和运维成本、秒级的生命周期管理、原生的 API 和安全能力，进而打造 Serverless K8S 底座，在 Serverless 大行其道的今天，其重要性不言而喻，亦是我们创造 KubeZoo 的初心。

<div align="center">
  <img src="docs/img/comparison.png" width=80% title="Comparison of Different Solutions">
</div>

您可以参考 [FAQ](./docs/faq.zh.md) 获得更多的信息。

## 前置依赖

请参考 [resource and system requirements](./docs/resource-and-system-requirements.md) 完成 KubeZoo 前置依赖检查。

## 部署

KubeZoo 当前基于 Kubernetes 1.36（`k8s.io/*` 全族锁定 1.36.3，staging 模块为 `v0.36.3`）。由于仍然引用了 `k8s.io/kubernetes` 的内部包，跨小版本升级是一次有意的移植而非改版本号。您可以采用如下方式部署 KubeZoo:


| Methods                     | Instruction                                | Estimated time |
| --------------------------- | ------------------------------------------ | -------------- |
| Deploy KubeZoo from scratch | [Deploy KubeZoo](./docs/manually-setup-cn.md) | < 2 minutes    |

## 社区

### 贡献

若您期望成为 KubeZoo 的贡献者，请参考 [CONTRIBUTING](CONTRIBUTING.md) 文档，我们也提供开发者手册 [guide](./docs/developer-guide.md) 供您参考。

### 联系方式

如果您有任何疑问，欢迎提交 GitHub issues 或者 pull requests，或者联系我们的 [Maintainers](./MAINTAINERS.md)。


## 协议

KubeZoo 采用 Apache 2.0 协议，协议详情请参考 [LICENSE](LICENSE)，另外 KubeZoo 中的某些实现依赖于 Kubernetes 代码，此部分版权归属于 Kubernetes Authors。