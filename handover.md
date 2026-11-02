# HCCS 网络支持交接

## 目标

为昇腾 910C/A3 的灵衢组网增加独立的 `accelerator_hccs` 网络类型，并让它和现有 `accelerator_rdma` 一样参与网络规划、部署和连通性测试。HCCS 使用 `hccn_tool -vnic -g` 返回的 vNIC 地址，不能把 `/etc/hccn.conf` 中的 RoCE 地址当作 HCCS rail 地址。

## 已完成

- 服务端网络字典新增 `accelerator_hccs`，链路层为 `hccs`，可绑定加速器卡 rail。
- Ascend agent 采集 HCCS vNIC IP、掩码和链路状态，并支持 `hccn_tool -hccs_ping`。
- 网络连通性测试按协议传递 `roce`/`hccs`，HCCS 测试不再调用 RoCE ping。
- 部署网络自动注入 HCCS 所需环境：
  - `ASCEND_GLOBAL_RESOURCE_CONFIG={"comm_resource_config.protocol_desc":["hccs:device"]}`
  - `ACL_OP_INIT_MODE=1`
  - `ASCEND_ENABLE_USE_FABRIC_MEM=1`
- HCCL PD 部署复用 HCCS 配置，并继续挂载 `/etc/hccn.conf` 供运行时读取。
- API、网络详情、UI 类型字典、rail 展示、中英文文档和翻译哈希已同步；未上报 HCCS vNIC 的旧 RoCE inventory 不多出 `hccs_gap` 字段。
- 加入 agent、netplan、部署、PD、网络测试、API 和浏览器 E2E 的覆盖用例。

## 验证

已通过：

- 相关 Go 单元测试及 targeted `-race`：agent、netplan、deploy、nettest、controller、network、API
- `go vet`（相关包）
- `make ui`
- `make check-translations`
- `make check-doc-links`
- `make check-architecture`
- `git diff --check`
- UI 全量测试（`make test-ui`）：2223/2223 通过，无跳过或失败
- E2E 覆盖静态检查：87/87 通过
- 网络类型/角色浏览器 E2E：中英文各 19/19 通过；产品字典移除 HCCS 的变异按预期失败，恢复后通过

## 真机复核

- live demo server 与两台 910C agent 均已升级到 `9118794aa`；旧 server 备份为 `/opt/gpuworks/server/gpuworks.bak-20261028-162633-hccs`。
- `910c_192.168.200.29` 上报 14 条 HCCS rail，`910c_k8s_192.168.200.30` 上报 16 条，vNIC 均为 `UP`；原有 RoCE 地址仍为 `DOWN`，两种地址没有混用。
- 已创建 `accelerator_hccs` 网络 `hccs-910c`（ID 4）并加载到两台节点。跨节点全卡矩阵 224/224 通过，同号卡复核 14/14 通过。
- 部署 462 的 decode/prefill 已按 HCCS 网络重建，三项 HCCS 环境变量均进入实例，日志确认 `hccs:device` 和 AdxlEngine 初始化成功；两个角色均为 1/1 running。
- 真实 chat 请求 `r294` 返回 HTTP 200，请求记录包含 `pd_prefill` 阶段，`fallback` 为空，确认 PD 交接未降级。

## 后续注意

- HCCS vNIC 地址来自 agent 实时 inventory；`/etc/hccn.conf` 中的 `10.20.x.x` 地址仍属于 RoCE 配置。
- 当前真机的 `hccn_tool` 输出与解析兼容；其他 CANN/驱动版本若输出格式不同，仍需补解析兼容性和回归记录。
