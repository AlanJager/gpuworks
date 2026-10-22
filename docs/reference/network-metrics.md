# 网络监控指标

本页是节点网络与 NIXL 传输指标的字典：名称、标签、口径与控制台展示位置。怎么配置网络见[配置节点网络](../how-to/configure-networks.md)；通用的指标查询与日志检索见[查看指标与检索日志](../how-to/monitor-and-logs.md)。

## 采集方式

agent 每 10 秒从 `/proc/net/dev` 和 sysfs 读取计数器，不起进程，用相邻两次读数的差除以间隔得到每秒速率。第一轮只建立基线、不出点；计数器回绕或清零的那一轮按 0 计。所有 `gpuworks_node_*` 指标都带 `node` 标签，下表只列其余标签。

只统计物理网卡、bond、网桥和 VLAN，回环与容器、虚拟化产生的虚拟接口不计；RDMA 只统计状态为 ACTIVE 的端口。

## 节点网络指标

| 指标 | 标签 | 说明 |
|---|---|---|
| `gpuworks_node_nic_{rx,tx}_bytes_per_sec` | `iface` | 每块网卡（物理网卡、bond、网桥、VLAN）的收发字节率 |
| `gpuworks_node_nic_errors_per_sec` | `iface`、`kind` | 网卡错误与丢包，`kind` 取 `rx_errors` / `rx_dropped` / `tx_errors` / `tx_dropped`；速率为 0 时不上报，只在变化时出点 |
| `gpuworks_node_rdma_{rx,tx}_bytes_per_sec` | `hca`（如 `mlx5_0:1`） | ACTIVE RDMA 端口的收发字节率，由 `port_rcv_data` / `port_xmit_data`（以 4 字节为单位）乘 4 得到 |
| `gpuworks_node_rdma_events_per_sec` | `hca`、`counter` | RDMA 错误计数（`symbol_error`、`link_downed`、`port_rcv_errors`、`port_xmit_discards`、`excessive_buffer_overrun_errors`）与 RoCE 拥塞计数（`np_cnp_sent`、`rp_cnp_handled`、`out_of_buffer`）；速率为 0 时不上报，只在变化时出点 |
| `gpuworks_node_role_{rx,tx}_bytes_per_sec` | `role` | 按角色的节点流量，从节点一侧计：`storage_sync` 由 agent 在节点间同步收发时计数；`serving` 由 server 网关在连到节点的连接上计数，服务网络直连和 agent 隧道都算 |

口径说明：

- 以太网上的参数流量无法从网卡计数中拆出，包含在网卡收发里；主机 RDMA 参数流量看 RDMA 端口计数。加速卡网口的流量不经过主机网卡，目前不采集，加速器 RDMA 网络的监控页只反映 bootstrap 网卡。
- 角色流量是节点上该角色的全部流量，不区分走的是哪个网络。

## NIXL 传输指标

vLLM 实例开启 NIXL（PD 分离）时，另外采集下列推理指标，由 vLLM 的 `vllm:nixl_*` 原生指标改名而来：

| 指标 | 类型 | 说明 |
|---|---|---|
| `gpuworks_infer_nixl_transfer_seconds` | 直方图 | 单次 KV 传输耗时；`_count` 为传输次数 |
| `gpuworks_infer_nixl_transfer_bytes` | 直方图 | 单次传输字节数；`_sum` 为累计传输字节 |
| `gpuworks_infer_nixl_failed_transfers_total` | 计数 | 失败的传输次数，含握手与通知失败 |
| `gpuworks_infer_nixl_kv_expired_total` | 计数 | KV 在被取走前已过期的请求数 |

PD 分离的部署与排查见 [PD 分离部署](../how-to/pd-disaggregation.md)。

## 在控制台哪里看

- 节点详情「网络」页的网卡清单和 RDMA 端口表显示最近两分钟内的实时收发速率。
- 网络详情页（含管理网）的「监控」标签页按节点画出该网络在各节点上的网卡收发、RDMA 收发（RDMA 网络，同一节点的多个 HCA 相加）、错误与拥塞，以及该网络承担的存储同步、服务角色的节点流量；时间范围可选 15 分钟到 24 小时。
