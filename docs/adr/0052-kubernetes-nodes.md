# ADR 0052: Kubernetes 是一种节点来源加一种运行时，卡按需向 kubelet 借用

日期：2026-10-19 · 状态：已接受

依据：[Kubernetes 接入计划 v2](../../artifacts/work/2026-10-15-kubernetes-nodes/plan-v2.md)（方案比较与 GPUStack、llm-d
对比见同目录 v1 与 design.html）。

## 背景

用户已有带 GPU 的 Kubernetes 集群，希望不逐台装 agent 就接入，并且 gpuworks 的工作负载在 Kubernetes 里可见、可记账。
server 里有 30 余处依赖“节点后面有一个 agent”，agent 承担探测、镜像、模型文件、健康检查、sleep、日志、终端与隧道。

## 决策

1. **agent 以 DaemonSet 运行，新增运行时 `kube`**：实例是钉在本节点的裸 Pod（`spec.nodeName`、`restartPolicy: Never`、
   host 网络、hostPath 同路径挂载、ownerReference 指向 agent 的 DaemonSet）。server 下发的期望状态不变，不知道执行载体
   是 Pod；与 Mac 的 host 运行时（ADR 0044）同构。不做 agentless 虚拟节点，不做 Operator / CRD。
2. **集群接入令牌换节点 token**：独立的 `POST /api/v1/kube/enroll`，按 `(集群, 节点名)` 幂等，首次使用时绑定集群
   （`kube-system` namespace 的 UID）；hub 握手不变。server 不持有 kubeconfig、不连 apiserver。
3. **每张卡一个分配者**：有 device plugin 时卡归 kubelet，gpuworks 为要用的卡起只申请 1 张卡的占位 Pod。device plugin
   不接受指定设备，所以借卡时一次为全部空闲卡各起一个占位 Pod（kubelet 没有选择余地），经 kubelet PodResources 认出
   每个占位 Pod 拿到的卡，退回不需要的；最后一个实例停止超过集群的空闲时间（默认 60 秒，2026-10-20 由 600 秒改定；-1 常驻）后归还。分给其他
   Pod 的卡作为外部占用上报，沿用“卡被平台外占用则不调度”的规则，调度器不改。
4. **只依赖标准库**：apiserver 用最小 REST 客户端（in-cluster ServiceAccount），PodResources 用标准库 h2c 与手写
   protobuf 解码；镜像操作委托 `k8s.io` 命名空间的 nerdctl。不引 client-go 与 gRPC。
5. **设备经 agent 自带的 device plugin，命令进节点的挂载命名空间**（2026-10-20 补充）：实例要的设备节点沿用各厂商
   docker / nerdctl 路径已验证的注入（`--device` 列表与驱动目录挂载）。agent 向 kubelet 注册每卡一个资源
   `gpuworks.ai/accelerator-<卡号>` 与各卡共用节点的 `gpuworks.ai/accelerator-shared`，实例 Pod 申请它要的，kubelet 经 Allocate 把设备
   节点交给容器；不要 privileged，不依赖厂商容器运行时与厂商 device plugin。按卡分资源名是因为 Allocate 不带 Pod 身份，
   只有资源名能指定是哪张卡。曾试过 Pod 注解 + CDI：kubelet 不把 Pod 注解交给容器，containerd 只认 device plugin 给的
   CDI，真机上设备并未注入（PPU 实例 `Failed to infer device type`）。agent 经 hostPID 以 `nsenter -t 1 -m` 在节点挂载
   命名空间运行 nerdctl、mount 与厂商 CLI：containerd socket、`certs.d`、exec 的 FIFO、NFS 挂载、厂商 CLI 依赖的 C 库都
   在节点自己的路径与版本上（PPU SDK 需要 glibc 2.38，agent 镜像是 2.36）。NVIDIA 不变：驱动可能在 GPU Operator 的容器
   里，CLI 由 RuntimeClass 注入。kubelet 用的 containerd 按 agent 容器的 shim 的 `-address` 认定，不按 socket 位置猜。
   一个集群条目一个厂商。device plugin 以非整卡分配（time-slicing、MIG、HAMi、vNPU）时不借卡，卡报为外部占用并带原因。
6. **生命周期跟随 Kubernetes**：节点 cordon 时平台同样 cordon；升级是把 agent DaemonSet 换到 server 同版本的镜像；离线
   15 分钟的节点经同集群在线 agent 确认已不存在且无实例时移除；节点自报引擎端口段，避开 NodePort 范围。

## 取舍

- **裸 Pod 而不是 Deployment**：重试、整组失败与排空已由 unit 对账负责（ADR 0029），再交给 Kubernetes 控制器会有两个
  对账循环争抢同一批 Pod。
- **host 网络**：端口分配、loopback 健康检查、隧道、serving 转发、NCCL/RDMA 选网卡全部原样可用；代价是实例不走 Pod
  网络与 Service。
- **借卡而不是关闭 device plugin**：不要求用户改动已有的 GPU Operator，k8s 账面如实；代价是借卡瞬间空闲卡会被短暂全部
  占用（其他 Pod 只会短暂 Pending），以及 time-slicing / MIG 副本的节点不能用。
- **privileged agent**：探测、NFS 挂载传播、进入节点挂载命名空间、注册 device plugin 需要；manifest 给 namespace 打 PSA `privileged` 标签。
- **复用厂商 device plugin 而不是自带**：gpustack 自带各厂商的 device plugin 并要求关掉厂商的；gpuworks 是集群的客人，
  沿用用户已装的插件，代价是只能按整卡借，切分资源的节点不能用。

## 后果

- 新增持久化事实：`kube_clusters` 表；`nodes` 的 `kube_cluster_id`、`kube_node_name`、`kube_unschedulable`；网络清单
  快照的 `engine_port_range`。
- 新包：`internal/server/kubemanifest`、`internal/agent/runtime/kube`（含 `kubeapi`、`podresources` 两个叶子包）。
- agent 镜像（`build/agent-image`，debian-slim：NVIDIA 运行时挂进来的 `nvidia-smi`、宿主机的 `npu-smi` 都是 glibc
  程序；只 COPY，amd64 与 arm64 都不需要模拟即可构建）。
- 新增持久化事实：`kube_clusters` 的 `vendor`、`state_dir`、`api_server`（manifest 参数）。agent 用节点的 DNS
  （`dnsPolicy: Default`），不依赖集群 DNS；没有组件提供 Service 地址的节点（KubeEdge、OpenYurt 边缘节点）用
  `api_server` 直连 API server。
