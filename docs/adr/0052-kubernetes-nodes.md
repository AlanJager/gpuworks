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
   每个占位 Pod 拿到的卡，退回不需要的；最后一个实例停止超过集群的空闲时间（默认 600 秒，-1 常驻）后归还。分给其他
   Pod 的卡作为外部占用上报，沿用“卡被平台外占用则不调度”的规则，调度器不改。
4. **只依赖标准库**：apiserver 用最小 REST 客户端（in-cluster ServiceAccount），PodResources 用标准库 h2c 与手写
   protobuf 解码；镜像操作委托 `k8s.io` 命名空间的 nerdctl。不引 client-go 与 gRPC。
5. **生命周期跟随 Kubernetes**：节点 cordon 时平台同样 cordon；升级是把 agent DaemonSet 换到 server 同版本的镜像；离线
   15 分钟的节点经同集群在线 agent 确认已不存在且无实例时移除；节点自报引擎端口段，避开 NodePort 范围。

## 取舍

- **裸 Pod 而不是 Deployment**：重试、整组失败与排空已由 unit 对账负责（ADR 0029），再交给 Kubernetes 控制器会有两个
  对账循环争抢同一批 Pod。
- **host 网络**：端口分配、loopback 健康检查、隧道、serving 转发、NCCL/RDMA 选网卡全部原样可用；代价是实例不走 Pod
  网络与 Service。
- **借卡而不是关闭 device plugin**：不要求用户改动已有的 GPU Operator，k8s 账面如实；代价是借卡瞬间空闲卡会被短暂全部
  占用（其他 Pod 只会短暂 Pending），以及 time-slicing / MIG 副本的节点不能用。
- **privileged agent**：探测、NFS 挂载传播、设备注入需要；manifest 给 namespace 打 PSA `privileged` 标签。

## 后果

- 新增持久化事实：`kube_clusters` 表；`nodes` 的 `kube_cluster_id`、`kube_node_name`、`kube_unschedulable`；网络清单
  快照的 `engine_port_range`。
- 新包：`internal/server/kubemanifest`、`internal/agent/runtime/kube`（含 `kubeapi`、`podresources` 两个叶子包）。
- agent 镜像（`build/agent-image`，debian-slim：NVIDIA 运行时挂进来的 `nvidia-smi` 是 glibc 程序）。
