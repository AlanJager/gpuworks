# ADR 0062: 经用户确认，server 用云厂商现发的短期 kubeconfig 把接入 manifest 应用到托管集群

日期：2026-10-22 · 状态：已接受（修订 [ADR 0052](0052-kubernetes-nodes.md) 第 2 条"server 不持有 kubeconfig、不连 apiserver"）

## 背景

托管 Kubernetes（阿里云 ACK、DigitalOcean DOKS）接入时，用户要自己找 GPU 节点标签、执行 `kubectl apply`。而云账号已经在
gpuworks 里，集群清单、节点池与集群凭据都能从云 API 拿到。ADR 0052 定下 server 不碰集群，是为了不保管长期有效、权限很大的
kubeconfig。

## 决策

1. **集群从云账号实时列出，不落库**：provider 驱动可实现 `ClusterProvider`（列集群与 GPU 节点池、签发 kubeconfig）。用户选一个
   GPU 节点池接入时，建一个普通的集群条目，条目只多记来源（云账号、集群 ID）；节点选择器按节点池生成。
2. **server 只在用户确认的那一次操作里连集群**："应用到集群"与"从集群卸载"都先做服务端 dry-run，列出每个对象将被创建、更新或删除，
   用户确认后才执行。每次操作向云 API 现取一份 kubeconfig（ACK、DOKS 都是 15 分钟有效），用完即弃，**不落库、不进日志**。
   只认 kubeconfig 里自带的证书或 token，要运行程序（exec、auth-provider）的 kubeconfig 拒绝。
3. **只应用 gpuworks 自己的 manifest**：server-side apply（`fieldManager=gpuworks`）`kubemanifest.Render` 渲染出的固定几种对象，
   其他 kind 一律拒绝。卸载先删 DaemonSet（agent、占位 Pod、实例 Pod 随之删除），同一集群里其他 gpuworks namespace 还在用的
   集群级对象（PriorityClass）保留。
4. **手动路径照旧**：每个条目仍有 `kubectl apply` 命令；API Server 没有公网地址的集群只能这样接入。手动建的条目没有来源，
   server 不会去连它。
5. **托管集群的节点不单独装 agent**：云主机列表标出所属集群，安装接口拒绝（`PROVIDER_INSTANCE_IN_CLUSTER`），避免与
   DaemonSet 抢同一台机器。

## 取舍

- 不保存 kubeconfig，换来的是每次操作多一次云 API 调用，以及云账号必须有签发集群凭据的权限（ACK 还要求子账号在集群 RBAC 里
  有权限）。
- 不做"自动同步"或后台对账：集群变化（节点池扩缩）由 DaemonSet 自己跟随，server 不需要常连。
- 不引 client-go：manifest 只有九种对象，最小 REST 客户端足够，与 agent 侧的做法一致（ADR 0052 第 4 条）。
