# ADR 0027: RDMA 网络由推断加节点固定代替选择器，测试工具优先宿主机、其次内置

**Status**: Accepted
**Date**: 2026-09-28

## Context

[ADR 0030](0030-network-objects.md) 给 RDMA 网络设了与以太网络相同的选择器：bootstrap 网段、网卡名通配、HCA 通配。2026-09-28 在 gpu3 上建参数网络时，这套选择器暴露了三个问题（记录见 `artifacts/archive/2026-09-28-rdma-zero-config/evidence-2026-09-28-lab.md`）：

- 选择器全空时，HCA 取全部已激活端口，bootstrap 却为空，没有任何提示，内部测试在 3 毫秒内全部失败。
- 用户要懂 bootstrap、HCA、RoCE v2 GID 与网卡 IP 的关系才能填对；把 1G 管理口的地址删掉并不能把它排除出 rail。
- 推断所需的事实（网卡地址与速率、RDMA 端口的状态、链路层与对应网卡、管理网地址）agent 早已上报。

同一天的实测给出了推断的依据：bootstrap 可以不在 RDMA 网卡上（H200 用 `eth0` 握手、8 条无地址的 IB rail 跑满 1.3 Tb/s 以上）；InfiniBand 端口不需要 IP；RoCE 端口没有 IPv4 地址时只能退到 RoCE v1，而 NCCL 2.21+ 与 UCX 用 RoCE v2。

## Decision

1. **RDMA 网络不再有 HCA 通配和网卡名通配**（迁移 106 删除 `networks.hca`、清空 RDMA 网络的 `interface`）。网段保留，含义是可选的 bootstrap 网段。
2. **推断规则只有一处**：`internal/server/netplan/rdma.go`。HCA：已激活端口，有 IB 取 IB、否则取有 IPv4 地址的 RoCE，只留速率最高的一组。bootstrap：节点固定值 → 网络的 bootstrap 网段（未命中留空并告警，不回退）→ 第一条 HCA 网卡的地址 → 无 IPoIB 的 IB 用管理网地址。
3. **用户的修改落在节点加载项上，推断结果不落库**。加载项的 `hcas` 为空表示推断，非空表示固定；`interface`/`ip` 在 RDMA 网络上是 bootstrap 固定值。第一次增删 HCA 时把当时的列表写入并固定，"恢复自动"清空。移除 HCA 默认作用于网络上所有用它的节点，因为 rail 要跨节点配对。节点最后一条 HCA 不能移除。经标签加载的节点被修改时生成节点加载项，沿用"节点优先于标签"。解析保持按需计算（ADR 0030 决策 3）：100 个节点约 6 ms。
4. **内部测试按 rail 报告**：rail 按 HCA IP 同子网优先配对、其余按序；节点对分通过 / 部分通过 / 失败；失败带稳定的原因码与所涉节点一侧，文案由客户端按语言渲染；RoCE 端口显式使用 IPv4 RoCE v2 GID，找不到时直接以此失败。另有"环形"测试方式（N 对、一轮）。
5. **测试工具的来源依次是宿主机 `ib_write_bw`、agent 内置的 `ib_write_bw`、提示安装 perftest**。内置版本从上游源码构建（`build/runtime-assets/perftest/`），动态链接宿主机的 `libibverbs` 与 `librdmacm`，不带厂商 direct-verbs 库，关闭 `ibv_wr_*` 发送接口。

## Consequences

- 创建 RDMA 网络不需要填任何东西；昇腾（hns RoCE、点对点 /30）与 Nebius H200（8×400G IB）两套实验环境按规则推断的结果与手工配置的正确结果一致。
- 推断会把速率相同的存储 RDMA 口也选进来，靠用户在节点列表里删掉；固定后的节点不再自动跟随新出现的端口，靠 `hca_unusable` 等告警和"恢复自动"兜底。
- 内置工具只覆盖"装了 RDMA 用户态库但没装 perftest"的主机；没有用户态库的主机仍需安装 perftest（包管理器会一并装上库）。内置版本引入 `libpci.so.3` 依赖与 `IBVERBS_1.12`（rdma-core 33+）下限，由 `make check-perftest` 守护。
- 部署侧不设置 NCCL 的 GID index：NCCL 2.21+ 自动选择的 RoCE v2 IPv4 GID 与测试一致；更早版本的自定义镜像需自行设置 `NCCL_IB_GID_INDEX`。
