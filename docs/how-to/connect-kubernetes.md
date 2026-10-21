# 接入 Kubernetes 集群

**适用场景**：已有带 GPU 的 Kubernetes 集群，希望把其中一批 GPU 节点交给 gpuworks 部署模型，而不逐台安装 agent。

公有云托管集群（阿里云 ACK、DigitalOcean DOKS、Nebius mk8s）的逐步操作见 [接入公有云托管 Kubernetes](connect-managed-kubernetes.md)。

**前置条件**：

- server 已经启动，集群节点能访问 server 地址（agent 主动外连，不需要节点入站端口）。
- 集群使用 containerd（kubeadm、k3s、RKE2、microk8s、GPU Operator 环境都是）。CRI-O（OpenShift）与 Docker 节点不支持，
  agent 启动时会拒绝并写明原因。
- 一个集群条目对应一个厂商的节点：NVIDIA、昇腾、海光或 PPU。NVIDIA 节点的 NVIDIA 容器运行时可用：或者是 containerd 的
  默认运行时（托管 Kubernetes 如 DOKS、Nebius、ACK 都是），或者有名为 `nvidia` 的 RuntimeClass（k3s 会自动创建）；
  其他厂商只要求节点上装好厂商驱动，不需要厂商的容器运行时，也不需要厂商的 device plugin。
- 要挂 NFS 存储的节点装有 `mount.nfs`（nfs-utils / nfs-common）：agent 在节点的挂载命名空间里用它挂载。
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
plugin 时，gpuworks 直接使用节点上的卡。device plugin 以非整卡方式分配时（time-slicing / MPS 把一张卡报成多份、MIG、
HAMi、昇腾 vNPU 等切分资源），一份不等于一张卡，gpuworks 不借卡：节点的卡全部显示为外部占用，原因写在卡的占用信息里，
不会在上面放实例；集群改回整卡分配后自动恢复。

实例要的设备节点（昇腾的 `/dev/davinci*`、海光的 `/dev/kfd`、`/dev/dri/*`、PPU 的 `/dev/alixpu*` 等）由 agent 自带的
device plugin 交给实例：每张卡一个扩展资源 `gpuworks.ai/accelerator-<卡号>`，各卡共用的控制节点是 `gpuworks.ai/accelerator-shared`，
实例 Pod 申请它用的卡与共用资源，kubelet 只把这几个设备节点给容器，实例不需要 privileged。哪些节点属于哪张卡取自厂商
本身的设备注入，与普通节点上的 docker / nerdctl 实例一致。需要插件之外设备的实例（例如 InfiniBand 的
`/dev/infiniband`）以 privileged 运行。

agent 用节点自己的 DNS（`dnsPolicy: Default`），与直接装在主机上的 agent 一样，不依赖集群 DNS；server 地址因此要是
节点能解析的地址，不能写集群内的 Service 名。agent 在节点的挂载命名空间里运行 nerdctl（拉取镜像、导入、终端）和 `mount`（NFS），所以 containerd 的 socket 与
镜像仓库配置（k3s 的 `registries.yaml` 生成的 `certs.d`）都按节点自己的位置使用，不需要配置。节点上同时跑着 Docker
自带的 containerd 时，agent 按自己容器的 shim 认出 kubelet 用的那一个。

## 接入

1. 在「Kubernetes」页点「接入集群」，填名称、server 地址（集群节点访问 server 用的地址）和厂商，按需填节点选择器
   （例如 `nvidia.com/gpu.present=true`，留空表示所有节点）。同一个 Kubernetes 集群里有两个厂商的节点时，按厂商建两个
   条目，各用不同的 namespace 与节点选择器。
2. 「高级」里的选项：
   - 状态目录：agent 在节点上的目录，默认 `/opt/gpuworks`。节点上已经装着一个 gpuworks agent 时换一个目录（例如
     `/opt/gpuworks-kube`），两者互不干扰。
   - containerd socket：留空时 agent 自己找（containerd、k3s、RKE2、microk8s 的默认位置）；装在别处时填写。
   - API server 地址：只给节点上没有组件提供 Service 地址的集群用（没有 kube-proxy 或其替代品，例如 KubeEdge、
     OpenYurt 的边缘节点），写成 `https://host:port`，地址须在 API server 的证书里；留空时 agent 经集群的
     `kubernetes` Service 访问。
   - 镜像拉取凭据：agent 镜像放在需要登录的仓库（企业 Harbor、云厂商的私有仓库）时，先在命名空间里建一个
     docker-registry 类型的 Secret（`kubectl -n gpuworks-system create secret docker-registry regcred …`，namespace 不存在时
     先建），再在这里填它的名称。agent Pod 用它拉镜像，agent 起的占位 Pod 也沿用它。
   - RuntimeClass：只对 NVIDIA：实例 Pod 访问 GPU 用的 RuntimeClass。留空（默认）时每个节点的 agent 自己判断：集群里有
     `nvidia` RuntimeClass 就用它，没有就用节点的默认运行时；agent 自己的容器里没有 `nvidia-smi` 时在节点上运行它。
     只有 RuntimeClass 叫别的名字时才需要填写。
   - 引擎端口段：默认 `40000-42767`。不要与集群的 NodePort 范围（默认 `30000-32767`）重叠：kube-proxy 会把发往本机
     地址的 NodePort 流量转走。
   - 空闲归还：借来的卡在最后一个实例停止后保留多久，默认 60 秒（每 30 秒检查一次，实际在 60 到 90 秒之间归还）；「常驻」表示一直不还，节点池整体交给 gpuworks。
3. 复制生成的命令在集群里执行：

   ```bash
   kubectl apply -f 'https://server.example/kube/manifest.yaml?token=…'
   ```

   被选中的节点会自动出现在节点页，带「K8s · 集群/节点名」标签；扩容出来的节点同样自动加入。创建 5 分钟后仍没有
   节点加入时，列表上的「排查」给出要执行的 `kubectl` 命令和常见原因（拉不到 agent 镜像、RuntimeClass 不存在、节点
   选择器没选中节点、agent 连不上 server）。

命令里带着集群的接入令牌，只给管理员看。吊销接入令牌后新节点不能再加入，已加入的节点不受影响。

## 日常运维

| 场景 | 做法 |
|---|---|
| 维护节点 | 在 Kubernetes 里 `kubectl cordon`：gpuworks 在下一次状态上报（一分钟内）跟着停止往该节点放新副本，`uncordon` 后同样恢复 |
| 升级 agent | 节点详情里点「升级」：把该集群的 agent DaemonSet 换到与 server 同版本的镜像，Kubernetes 依次替换各节点的 agent，实例 Pod 不受影响。也可以自己 `kubectl -n gpuworks-system set image ds/gpuworks-agent agent=<镜像>` |
| 节点被删除或缩容 | 节点离线 15 分钟后，gpuworks 请同集群另一台在线节点确认；Kubernetes 里已没有该节点且其上没有实例时自动移除 |
| 从 gpuworks 删除节点 | 只删除记录，不卸载 agent；DaemonSet 仍选中该节点时它会重新加入，需要先调整节点选择器 |
| 卸载 | `kubectl delete -f` 同一个 manifest 地址：DaemonSet、实例 Pod、占位 Pod 一起删除，卡还给 Kubernetes |

## 限制

- 只支持 containerd：镜像由 agent 经节点的 containerd（kubelet 的 `k8s.io` 命名空间）拉取与导入，不经过 kubelet，
  所以实例镜像不用 Pod 的 `imagePullSecrets`，私有仓库的凭据配在 gpuworks 的镜像来源里（集群条目的镜像拉取凭据只管
  agent 镜像）。
- 实例使用 host 网络，不创建 Service / Ingress，经 gpuworks 网关访问。
- 厂商的命令行工具（`npu-smi`、`hy-smi`、`ppu-smi`）在节点的挂载命名空间里运行，用节点自己的 C 库与驱动库，所以 agent
  镜像与节点的发行版无关。
- agent 以 privileged、hostNetwork、hostPID 运行（探测、经节点挂载命名空间运行 nerdctl 与 mount 需要）。
