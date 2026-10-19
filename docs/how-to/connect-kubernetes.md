# 接入 Kubernetes 集群

**适用场景**：已有带 GPU 的 Kubernetes 集群，希望把其中一批 GPU 节点交给 gpuworks 部署模型，而不逐台安装 agent。

**前置条件**：

- server 已经启动，集群节点能访问 server 地址（agent 主动外连，不需要节点入站端口）。
- 集群使用 containerd（k3s、kubeadm、GPU Operator 环境都是）；节点上的 NVIDIA 容器运行时可用，通常已有名为
  `nvidia` 的 RuntimeClass（k3s 与 GPU Operator 会自动创建）。
- 能在集群里执行 `kubectl apply`，并允许一个 `privileged` 的 namespace（manifest 会给 namespace 打上
  Pod Security Admission 的 `privileged` 标签；Kyverno、Gatekeeper 一类的策略需另外放行）。

## 工作方式

一个集群以一个 DaemonSet 接入。被选中的每个节点上跑一个 agent，它用集群的接入令牌换取自己的节点 token，之后与
普通节点完全一样：调度、部署、网关、日志、终端都不区分。实例落成钉在该节点上的 Pod（`restartPolicy: Never`、
host 网络、模型目录以 hostPath 同路径挂载），`kubectl get pods -n gpuworks-system` 能看到每个实例；重试与重建仍由
gpuworks 负责，Kubernetes 不会把失败的实例拉起来。

每张卡同一时刻只有一个分配者。节点上装了 device plugin（`nvidia.com/gpu` 等）时，卡归 kubelet 分配：gpuworks 要用
哪张卡，就为它起一个只申请 1 张卡的占位 Pod（`gpuworks-hold-*`），实例全部停止并超过集群的「空闲归还」时间后删除
占位 Pod，把卡还给 Kubernetes。被其他 Pod 占着的卡在 gpuworks 里显示为外部占用，不会被调度。节点没有 device
plugin 时，gpuworks 直接使用节点上的卡。

## 接入

1. 在「Kubernetes」页点「接入集群」，填名称与 server 地址（集群节点访问 server 用的地址），按需填节点选择器
   （例如 `nvidia.com/gpu.present=true`，留空表示所有节点）。
2. 「高级」里的选项：
   - containerd socket：k3s 是 `/run/k3s/containerd/containerd.sock`，其余一般是默认的 `/run/containerd/containerd.sock`。
   - RuntimeClass：注入 NVIDIA 设备的 RuntimeClass，默认 `nvidia`；节点默认运行时已是 NVIDIA 的可以清空。
   - 引擎端口段：默认 `40000-42767`。不要与集群的 NodePort 范围（默认 `30000-32767`）重叠：kube-proxy 会把发往本机
     地址的 NodePort 流量转走。
   - 空闲归还：借来的卡在最后一个实例停止后保留多久，默认 600 秒；「常驻」表示一直不还，节点池整体交给 gpuworks。
3. 复制生成的命令在集群里执行：

   ```bash
   kubectl apply -f 'https://server.example/kube/manifest.yaml?token=…'
   ```

   被选中的节点会自动出现在节点页，带「K8s · 集群/节点名」标签；扩容出来的节点同样自动加入。

命令里带着集群的接入令牌，只给管理员看。吊销接入令牌后新节点不能再加入，已加入的节点不受影响。

## 日常运维

| 场景 | 做法 |
|---|---|
| 维护节点 | 在 Kubernetes 里 `kubectl cordon`：gpuworks 跟着停止往该节点放新副本，`uncordon` 后恢复 |
| 升级 agent | 节点详情里点「升级」：把该集群的 agent DaemonSet 换到与 server 同版本的镜像，Kubernetes 依次替换各节点的 agent，实例 Pod 不受影响。也可以自己 `kubectl -n gpuworks-system set image ds/gpuworks-agent agent=<镜像>` |
| 节点被删除或缩容 | 节点离线 15 分钟后，gpuworks 请同集群另一台在线节点确认；Kubernetes 里已没有该节点且其上没有实例时自动移除 |
| 从 gpuworks 删除节点 | 只删除记录，不卸载 agent；DaemonSet 仍选中该节点时它会重新加入，需要先调整节点选择器 |
| 卸载 | `kubectl delete -f` 同一个 manifest 地址：DaemonSet、实例 Pod、占位 Pod 一起删除，卡还给 Kubernetes |

## 限制

- 只支持 containerd：镜像由 agent 经节点的 containerd（kubelet 的 `k8s.io` 命名空间）拉取与导入。
- 实例使用 host 网络，不创建 Service / Ingress，经 gpuworks 网关访问。
- device plugin 开了 time-slicing 或 MIG 副本时，“一张卡”不再是物理卡，借卡结果不可预期，先不要在这类节点上使用。
- agent 以 privileged、hostNetwork、hostPID 运行；昇腾、PPU 等需要设备节点的工作负载以 privileged Pod 运行。
