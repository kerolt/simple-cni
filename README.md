# simple-cni

## 需要的配置文件

- 插件加载参数：`/etc/cni/net.d/00-simplecni.conf`
- 当前节点使用的子网信息：`/run/simple-cni/subnets.json`
- 插件分配的网络配置信息存储位置：`/var/lib/cni/networks/simple-cni/<cni-name>.json`

这里需要解释一下几个配置文件为什么要放不同的目录下：

1. 启动阶段（配置加载）

    插件从 /etc/cni/net.d/ 读取网络配置（例如网段、路由方式等）。

2. 运行阶段（状态生成）

    插件运行时，会在 /run/simple-cni/ 写入当前节点或进程级的运行时状态，如：

    - 当前分配的子网（subnets.json）；
    - 其他节点同步信息。

    这些数据是临时的，重启后可重新计算。子网并不是一成不变的，可能节点重启后或者网络分配时会改变，所以不应该放在 `/var/lib/` 下持久化。

3. 分配阶段（状态持久化）

    插件每当分配一个 IP，会在 `/var/lib/cni/networks/<netname>/` 中持久化：

    - 哪个容器用了哪个 IP；
    - 最后一个分配的地址（用于下次递增）。

    这些数据必须持久化，否则重启后会出现 IP 重复分配冲突。

## 启动节点

利用 kind 模拟启动一个 master 节点，三个 worker 节点：

```sh
kind create cluster --config deploy/kind.yml
```

## 加载镜像到节点中

```sh
kind load docker-image kerolt/simplecni:v0.1
kind load docker-image alpine:<tag>
```

## 部署 simple-cni

```sh
kubectl apply -f deploy/simple-cni.yml
```

## 部署多副本 Deployment

```sh
kubectl create deployment cni-test --image=alpine:<tag> --replicas=6 -- top
```
