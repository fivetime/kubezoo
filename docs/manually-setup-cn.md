## 快速开始

本文介绍如何在 Kubernetes 集群上部署 KubeZoo. 虽然 KubeZoo 可以对接任何标准的 Kubernetes 集群，但是作为样例,
本文会以 `kind` 集群作为上游集群.


> ⚠️ **这里描述的只是三个组件之一。** `make local-up` 起的是 kubezoo-gateway,
> 并且**在能找到同级的 [kubezoo-controller](https://github.com/fivetime/kubezoo-controller)
> 检出时**一并部署它。没有控制器的话,集群会**接受 Tenant 对象然后什么都不做** ——
> 没有 namespace、没有 RoleBinding,**也没有任何报错指向缺了什么**。
> 准入策略是第三块,来自
> [kubezoo-contract](https://github.com/fivetime/kubezoo-contract);
> 缺了它,租户可以点名平台的任意 RuntimeClass 从而跑出沙箱。
>
> 检出目录不在同级时,用 `KUBEZOO_CONTROLLER_DIR` 和 `KUBEZOO_CONTRACT_DIR` 指定。

### 前置条件

请安装最新版本的 
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [yq](https://github.com/mikefarah/yq#install) 
- [cfssl](https://github.com/cloudflare/cfssl#installation)

### 创建本地环境
通过运行下面的命令在本地构建 kubezoo 的环境
```console
make local-up
```

当所有流程都准备就绪后，kubezoo 将运行在本地的 6443 端口，请确保该端口没有被别的程序占用，你会看到以下输出
```
Export kubezoo server to 6443
Forwarding from 127.0.0.1:6443 -> 6443
Forwarding from [::1]:6443 -> 6443
```

### 访问 KubeZoo!

```console
$ kubectl api-resources --context zoo
NAME                              SHORTNAMES   APIVERSION                        NAMESPACED   KIND
...
tenants                                        tenant.kubezoo.io/v1alpha1       false        Tenant
```

### 创建一个租户

```console
$ kubectl apply -f config/setup/sample_tenant.yaml --context zoo
tenant.tenant.kubezoo.io/111111 created
```

租户名称必须是有效的6字符[RFC 1123][rfc1123-label]DNS标签前缀(`[A-Za-z0-9][A-Za-z0-9\-]{5}`)。

### 获取租户的 kubeconfigs 文件

```console
$ kubectl get tenant 111111 --context zoo -o jsonpath='{.metadata.annotations.kubezoo\.io\/tenant\.kubeconfig\.base64}' | base64 --decode > 111111.kubeconfig
```

### 以租户的身份创建一个 pod

```console
$ kubectl apply --kubeconfig 111111.kubeconfig -f- <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test
spec:
  containers:
  - name: test
    image: busybox
    command:
    - tail
    args:
    - -f
    - /dev/null
    resources:
      limits:
        cpu: 0.5
        memory: '0.5Gi'
      requests:
        cpu: 0.5
        memory: '0.5Gi'
EOF
pod/test created
```

以租户的身份获取 pod

```console
$ kubectl get po --kubeconfig 111111.kubeconfig
NAME   READY   STATUS    RESTARTS   AGE
test   1/1     Running   0          44s
```

以集群管理员的身份获取 pod

```console
$ kubectl get po -A
NAMESPACE            NAME                                         READY   STATUS    RESTARTS   AGE
111111-default       test                                         1/1     Running   0          2m28s
default              kubezoo-0                                    1/1     Running   0          34m
default              kubezoo-etcd-0                               1/1     Running   0          34m
default              test                                         1/1     Running   0          2m41s
...
```

[rfc1123-label]: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#dns-label-names
