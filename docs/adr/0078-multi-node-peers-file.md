# ADR 0078: 多机部署的每个 rank 带一份只读 peers 文件

日期：2026-11-19 · 状态：已接受

## 背景

多机部署的启动变量只给一个 rank 自己的地址和 leader 的地址（`NODE_ADDR`、`MASTER_ADDR`、`MASTER_PORT`）。vLLM、SGLang 的 rendezvous 只要这些就够；但不少配方要在启动前知道同组每个 rank 的位置：pico-dgx-ds4 的 `fleet.conf` 要每个 rank 的主机名、管理地址和每条 RDMA 链路的 HCA 名与 RoCE 地址，昇腾 HCCL 的 rank table、Mooncake 的配置同理。没有平台给的名单，配方只能在容器里自建一轮发现（互相广播、共享存储上交换文件），既慢又各写一套。

这些事实 server 在放置一组时都已经有了：组里有哪些节点、各自的 rank，组共用的参数网络在每个节点上解析出的地址、bootstrap 网卡和 rail（`netplan.Resolved`），以及 agent 上报的网络清单（RDMA 端口的网卡与经 GID 表核实的 RoCE 地址、加速卡端口地址）。

## 决策

1. **一份带类型、带版本的文档，沿用已有词汇**：`proto.PeersFile`。顶层 `version`、读者自己的 `node_rank`、`nnodes`、`master_addr`/`master_port`、组共用网络的 `type`/`link_layer`、按 rank 排列的 `ranks`；每个 rank 有 `node_rank`、`node_uuid`/`node_name`/`hostname`、`node_addr`、`management`（`interface`、`ip`，同 agent 上报的管理地址）、bootstrap `interface`，以及参数网络的 `hcas`（每条 rail 的 `name`、`device`/`port`、`netdev`、`ip`，同网络接口对 rail 的描述）。字段名是启动变量的小写与网络接口的字段名，不另起名字；凡是同时作为启动变量存在的值，在每个 rank 上都与该 rank 的启动变量相等（读者自己那一行的 `node_addr` 就取本 rank 的 `NODE_ADDR`，两者出自同一个函数）。只新增字段不升 `version`，字段含义变化才升。形状写在 [部署启动参数来源](../reference/deployment-launch-sources.md#多机-peers-文件gpuworks_peers_file)。
2. **在放置时生成，随启动快照保存**：controller 在规划一代（generation）的各 rank 之前，用这一代的节点和已经解析好的组网络生成名单（`planning.GroupPeers`），经拓扑进入启动参数，`deploy.ResolveLaunch` 把它与本 rank 的序号、rendezvous 组成 `ResolvedLaunch.Peers`。文档是期望状态的一部分，与 argv、env 一样随实例保存，重推、重连时不重新推导，容器看到的始终是这一代放置时的事实。评估预览按同样方式生成。
3. **路径是启动变量，也是同名 env**：`${GPUWORKS_PEERS_FILE}` 与 `${GPUWORKS_NODE_RANK}` 一样是平台事实（`placement.VarPeersFile`，resolver 不能返回），在模板的 args、env、准备脚本与手写命令里展开；同名同值的系统层 env `GPUWORKS_PEERS_FILE` 让配方启动的程序不经传参也能找到文件。没有 peers 文件的启动在模板字段里引用它会报错，与单机引用 `MASTER_ADDR` 相同。
4. **agent 写文件、只读挂载在平台目录**：agent 把文档写进实例自己的运行时挂载目录（purpose `peers`），只读挂载到 `/gpuworks/peers.json`；每次启动容器重写，回收实例时删除。所有运行时都走同一个挂载（docker、nerdctl、udocker、Kubernetes 的卷，host 进程运行时按挂载表把容器路径换成主机路径），所以凡支持多机的运行时都能用。写失败以 `PEERS_FILE_PREPARE_FAILED` 判这次启动失败，不起容器。
5. **`/gpuworks` 归平台，是同一个容器路径空间里的一项**：平台提供的文件（peers 文件、准备脚本 wrapper）都在容器的 `/gpuworks` 下（`proto.PlatformFileDir`）。它不另立检查，而是 [ADR 0077](0077-template-managed-writable-dirs.md) 那条路径规则（`proto.ContainerPathsOverlap`）里多出的一项：模板导入时它与主模型挂载点、组件 `mount_path`、资产 `unpack_to`、dir 的 `mount_path` 一起互查（`CATALOG_MOUNT_PATH_CONFLICT`），启动解析时所有挂载都不能与它重叠（`MOUNT_TARGET_RESERVED`）。按原样重启的部署保留创建时已有的挂载，除非它会盖住这次的 peers 文件。以后新增的平台文件放在同一目录，不会与配方的挂载相撞。
6. **按能力下发，规则与托管目录相同**：peers 文件只有 agent 能写，所以用到它的启动按 ADR 0077 的 agent 能力规则处理（能力 `launch.peers_file`）。模板在启动字段里引用 `${GPUWORKS_PEERS_FILE}`（启动字段两种拼写都算，手写手工命令只算带前缀的）就是用到它：旧 agent 的节点以 `agent_capability_missing` 拒绝，而不是在启动时报变量未解析。没有引用它的多机 rank 在旧 agent 上照常启动，只是没有文件、变量与 env，免得路径指向一个不存在的文件，也不让多机部署因为一个它不读的文件而放不上去；名单本身仍列出该 rank。判断在 planning 里做一次（`peersForAgent`），controller 与评估预览共用。
7. **不写容器自己探测的值**：RoCE GID index 等取决于容器内设备视图的值不写，由配方在容器里探测。

## 取舍

- **期望状态里多一个 JSON 键**：名单可以从节点与网络表推导，但推导要在放置那一刻做；之后节点清单或网络变化时重新推导，会让同一个运行中的容器看到与其他 rank 不一致的名单。它和 argv、env 一样属于这次启动的事实，所以随 `ResolvedLaunch` 保存，形状只有 `proto.PeersFile` 一个类型。
- **文件而不是一组 env**：rank 数与 rail 数可变，展开成 env 要发明编号约定；JSON 一份即可被任意语言读取，也方便配方转换成自己的格式。
- **复用 rail 描述**：rail 的网卡与地址取 `netplan.Rails`，与网络页、NCCL 注入用的是同一份 rail 解释，不另写一套清单解析。
- **每个 rank 一份、只差 `node_rank`**：也可以让各 rank 文件完全相同、由 `NODE_RANK` 区分，但把读者的 rank 写进文件，读文件的程序就不必再拿一个参数。

## 后果

- 协议：`ResolvedLaunch.peers`（`proto.PeersFile`），agent 能力 `launch.peers_file`，失败码 `PEERS_FILE_PREPARE_FAILED` 与 `MOUNT_TARGET_RESERVED`；启动变量 `${GPUWORKS_PEERS_FILE}`（别名 `${PEERS_FILE}`）；平台目录 `proto.PlatformFileDir`。
- 没有新增数据库列或表；文档随实例已有的期望状态保存。
- 单机部署、以及运行在不宣告该能力的 agent 上的 rank，没有 peers 文件。
