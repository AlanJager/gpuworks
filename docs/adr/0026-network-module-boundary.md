# ADR 0026: 网络决策集中在纯叶子包与一个服务，网络目录经接口注入

**Status**: Accepted
**Date**: 2026-09-27

## Context

节点网络（[ADR 0030](0030-network-objects.md)）的解析、管理网兜底、参数组选网、存储同步路径、能否承载服务等决策原先写在 `store` 包里，经 `*store.Store` 上的七个方法被 api、controller、hub、planning、proxy 直接调用；角色 / 类型词表也定义在 store，校验分散在 api 与 store，store 装配 agent 期望状态时还会自己调用解析。结果是：

- 没有一个接缝能回答“某角色走哪张网”，也就无法替换实现；
- store 既存数据又做领域决策；
- 同一条规则有多处实现（角色规范化在 api 与 store 各一套、顺序不同；“解析结果 → 参数网络启动参数”在部署预览、真实启动、NCCL 测试里各写一遍）。

产品上还考虑把复杂网络（自定义网络、专用服务 / 存储 / RDMA 参数网络、内部测试）做成高级功能，普通版只走管理网与 agent 隧道。拿掉所有网络对象后，现有解析天然退回管理网，缺的只是一个干净的边界。

## Decision

1. **`internal/server/netplan` 是纯叶子包**，只依赖 `proto`：词表、领域模型（解码后的 Network / Attachment / Node）、`Resolve`、管理网、参数组、存储同步路径、能否承载服务、服务转发地址，以及内部测试覆盖哪些角色。所有网络决策只在这里实现一次。
2. **`internal/server/network` 是唯一入口**：从 store 读节点与网络行，`convert.go` 是 store 行 → netplan 模型的唯一转换；对核心流程提供 `ComputeGroup`、`StoragePath`、`CanServe`、`ServingIP`、`ServingEndpoints`、`NodeNetworks`、`Endpoints`、`State`；网络、挂载与管理网角色的校验和变更也在这里。
3. **store 只做持久化**：保存网络、挂载、测试记录与节点列，不解析、不持有词表、不校验角色；由架构检查强制 store 不依赖 netplan / network。服务网络地址不再由 store 写进期望状态，改由 hub 推送时向网络服务询问。
4. **使用方声明窄接口**：hub、controller、proxy、planning 各自声明需要的方法，`app` 创建唯一的 `network.Service` 注入全部组件（api 经 `Deps.Networks`），保证所有组件读同一份目录。
5. **网络目录经 `network.Catalog` 注入**：`StoreCatalog` 是库里全部网络与挂载；`ManagementOnly` 不声明任何网络，解析一律退回管理网与 agent 隧道，对网络与挂载的变更返回 `NETWORK_DEDICATED_UNAVAILABLE`，管理网的按节点角色开关仍可用。选择哪种目录只在 `app` 一处决定；目前固定为 `StoreCatalog`，是否以及如何做版本开关另行决定。

## Consequences

- 以后把复杂网络做成高级功能，只需在 `app` 选择目录实现；若要让普通版二进制完全不含高级代码，可在 `Catalog` 实现与网络页路由上加编译标签，核心流程不用改。
- api 多依赖 `network` 与 `netplan`，`nodeaddr` 收进 `planning.PeerHost`，`api_fanout` 净增 1；`Deps.Networks` 使 `api_deps_fields` 增 1；`network` 服务读 store 使 `store_concrete_importers` 增 1（见 `scripts/arch-constraints.md`）。
- 被关掉的管理网角色改按角色显示顺序存储（原为字母序）；已存的旧顺序读取时按集合比较，不会被误判为变更。

## Alternatives Considered

- **只在 API 与界面层按版本隐藏**：改动最小，但解析仍散在 store，核心流程没有可替换的接缝，复杂度也无法从普通版里隔离。
- **拆成独立仓库或插件**：Go 静态单二进制不适合插件，拆仓成本高，而核心流程仍需一个本地的管理网实现。
- **把网络 HTTP 处理拆成独立包挂路由**：对版本开关更彻底，但当前网络页的 DTO 组装与其他页面同在 api 层，先保持一致；需要按版本裁剪路由时再拆。
