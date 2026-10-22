# 启用和排查 dGPU

**适用场景**：想让多个模型实例共用一张物理卡，按显存上限隔离；或部署带 dGPU 显存限额的请求被拒绝，需要查 `status` / `reason`。

**前置条件**：

- 节点已接入并上报 GPU（见[接入节点](connect-nodes.md)）。
- NVIDIA：x86_64、glibc ≥ 2.17，宿主机驱动提供动态库（`libcuda.so.1`、`libnvidia-ml.so.1`）；昇腾：宿主机的 device-share 权限满足 vCANN 的要求；PPU：配额语义是显存限额加繁忙时的加权 QoS。各厂商的支持范围见[支持矩阵](../reference/support-matrix.md)。
- 想先走一遍完整流程，见[一张 GPU 跑两个模型](../tutorials/one-gpu-two-models.md)；语义与边界见 [dGPU 是什么、又不是什么](../explanation/dgpu.md)。

在部署请求中填写 dGPU 显存上限，平台根据节点硬件自动选择后端（实现分别是 NVIDIA 的 HAMi-core、昇腾的 vCANN 和 PPU 的 HGGC）。
能力状态由 server 推导，`status` 与 `reason` 是唯一对外来源。`status` 只有两种取值：

- `ready`：运行时、dGPU 资产、（非 NVIDIA 后端还需）dGPU 探针条件全部满足，可以创建带配额的实例。
- `unavailable`：任一条件不满足，`reason` 说明具体原因，例如 agent 版本过旧未上报探针、
  缺少 dGPU 资产、运行时未验证容器推理、探针尚未完成或探针失败；检查 agent 日志，不要退回
  整卡模式假装成功。

非 NVIDIA 后端另有独立的 `probe_status`（`passed`/`skipped`/`failed`），用于诊断 dGPU 探针本身
的结果，不等同于上面的 `status`。

NVIDIA 需要 x86_64、glibc ≥ 2.17 和驱动动态库；PPU 20/80 是繁忙时加权 QoS；昇腾设备共享
受宿主机权限影响，Gitee 云实例不能当作 dGPU 通过证据。详情见[支持矩阵](../reference/support-matrix.md)。

## 调整 dGPU 配额

在「GPU 设备」页的「dGPU」标签，切片所在行的「调整配额」按钮修改它所属部署的显存上限（API：
`PUT /api/v1/deployments/{id}/dgpu`，`{"vram_limit_gib": 12}`）。新上限**保存即生效**：

- 部署之后启动的实例按新上限启动。
- 运行中的实例当场改上限：调度记账按新上限计，agent 把新上限写进实例的 HAMi-core 共享区，HAMi 从下一次分配起按它限制，实例不重启。
- 引擎能不能用上新上限取决于引擎：vLLM、SGLang 在启动时按上限切 KV 缓存，到部署页**重启实例**后才会用上；按需分配显存的引擎直接用上。

以下实例保持原上限，到重启时才按新上限运行（GPU 页显示「重启后 X GiB」，返回的 `pending` 列出每个实例与原因）：

| `reason` | 情况 |
|---|---|
| `backend` | 切分后端只在启动时读上限（昇腾 vCANN、PPU HGGC） |
| `in_use` | 调低时，实例已占用的显存超过新上限——当场调低会让引擎下一次分配失败而退出 |
| `starting` / `asleep` | 调低时实例正在启动或休眠，之后需要的显存可能比现在多 |
| `no_usage` | 调低时实例没有上报显存用量 |

拒绝（原配额不变）：

| code | 含义与处理 |
|---|---|
| `DEPLOYMENT_NEVER_FITS` | 新上限下没有任何节点能跑这个部署（比如比卡还大）；与创建时的检查相同 |
| `DGPU_QUOTA_NO_ROOM` | 调高时，实例所在的卡在扣掉它自己的占用后放不下新切片（`params` 给出节点、卡号、可用与需要的 GiB）；调小上限，或先腾出这张卡 |
| `DGPU_QUOTA_NOT_PARTITIONED` | 部署使用整卡，没有 dGPU 配额 |
| `DGPU_QUOTA_NOT_EDITABLE` | 多机或 PD 部署逐 rank 放置，不在这里改 |
| `DGPU_QUOTA_INVALID` | 上限不是正数 |

部署时间线会记一条「dGPU 显存上限 A → B」及立即生效与待重启的实例数。实例因为用量顶到上限而退出时，失败原因写作
`dGPU memory limit reached on GPU N: the engine needed X GiB, its limit is Y GiB`。
