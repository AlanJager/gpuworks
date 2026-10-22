# PD 分离部署

**适用场景**：提示长（上千 token）、并发高，prefill 抢占 decode 导致输出不稳时，把推理拆成 prefill 与 decode 两组实例（可跨机）。

**前置条件**：

- 已熟悉普通部署流程，见[部署模型、选卡与解释调度结果](deploy-models.md)。
- 硬件与引擎符合所选 PD 模式：`vllm-nixl` 面向 NVIDIA，`vllm-ascend-mooncake` 面向昇腾，`sglang-nixl` 未验证。
- P、D 跨机时 KV 走参数网络：RDMA 网络传输最快，以太网可用，见[配置节点网络](configure-networks.md)；昇腾模式跨机时还要求两端在同一个加速器 RDMA 网络上，且 prefill 与 decode 所在 NPU 在 NPU 网络上互通。

PD 分离把一个模型的推理拆成两组实例：prefill 实例只算提示，decode 实例从它那里取走 KV 后逐 token 生成。对外仍是
一个部署、一个服务名，请求照常发给 `/v1`，由网关先后调用两组实例。设计与取舍见
[ADR 0028](../adr/0028-pd-disaggregated-deployment.md)。

## 何时值得用

- 提示长（上千 token）、并发高，prefill 抢占 decode 导致输出不稳时收益最明显；提示很短时 KV 传输的开销可能超过
  收益，服务页 PD 卡片的平均提示长度可以帮助判断。
- 跨机时 KV 走参数网络。RDMA 网络下传输最快；以太网下可以用，但每个请求多一次 KV 拷贝。
- 同一节点上的 P、D 各用各的卡，KV 走本机 TCP，适合验证，不代表跨机性能。

## 部署

1. 部署页的"节点放置"选 **PD 分离**，再选 PD 模式。可选模式来自引擎档位：vLLM 有 `vllm-nixl`（NVIDIA）与
   `vllm-ascend-mooncake`（昇腾），SGLang 有 `sglang-nixl`（未验证）。
2. 为 prefill 与 decode 各设 TP，并逐个添加实例：每个实例选一个节点，可选钉卡。模式有 TP 规则：`vllm-nixl` 要求
   decode TP ≥ prefill TP，`vllm-ascend-mooncake` 相反，`sglang-nixl` 要求相等。
3. 一个实例要跨多台节点时，在该实例下"添加节点"：第一台是 leader（对外服务），其余是 follower。此时必须写 TP，
   TP × PP 平均分到各节点。平台会等模型与镜像在每台节点上都就绪后才同时启动各 rank。
4. 选择 P 不可用时的行为：默认降级到 decode 本地完成（`fallback: decode`），也可以直接返回 503（`fail`）。
   decode 取 KV 失败时默认报错（`kv_load_failure: fail`），可改为本地重算（`recompute`）。
5. 昇腾模式的 KV 走 NPU 自带的 RoCE 网口：跨机部署时这些节点必须共用一个加速器 RDMA 网络（见
   [配置节点网络](configure-networks.md)），否则被拒绝；prefill 与 decode 所在的 NPU 还必须在 NPU 网络上互通。用
   `hccn_tool -i <卡号> -lldp -g` 看每个口的对端，`hccn_tool -i <卡号> -ping -g address <对端 IP>` 验证。
   没有交换机、两台机器按卡号直连时，只有两台机器的同号卡之间能通，P、D 要选同号卡；平台目前不检查这一点。
6. 预览按实例给出放置、显存与完整命令。KV Offload 只在 prefill 上生效，decode 不挂 KV 缓存层；不要在自定义参数里
   写 `--kv-transfer-config` 或 `--kv-offloading-size`，平台会拒绝。

API 形式：`POST /api/v1/deployments` 的 `placement.pd`，字段见 [API 参考](../reference/api.md)。

## 运行中

- **状态**：部署显示"未就绪 / 部分就绪 / 运行中"，按每个角色的期望数与就绪数推导；有意缩到 0 的 prefill 不算缺。
- **自检**：每个 prefill 实例首次就绪后，平台自动经网关做一次交接，在该实例的时间线记
  `PD_HANDOFF_VERIFIED`（传输方式为 rdma、tcp、local 或 unmeasured）；参数网络是 RDMA 却没有 RDMA 流量时记
  `PD_HANDOFF_NOT_RDMA`，交接失败记 `PD_HANDOFF_FAILED`。
- **扩缩**：部署详情的"调整角色"改变各角色的实例（`PUT /api/v1/deployments/{id}/pd`）。被去掉的实例先排空再停；
  prefill 可以缩到 0，此时请求全部由 decode 完成。模式、TP、PP 与 KV 加载失败策略启动后不能改，需要克隆部署。
- **重试一个实例**：一个实例连续失败达到上限后平台不再重建，部署详情会列出它的失败次数与最后一次原因，并给出
  "重试"：只清掉该实例的失败记录并重新创建，不影响其他实例。在"调整角色"里删掉一个实例，它的失败记录也随之清除，
  再加回来就重新计次。
- **镜像与网络变化**：新增或重建的实例按当前的镜像与参数网络启动，与在跑实例不同也不拦（例如逐个换成新镜像做
  灰度，或网络割接）；两边不兼容或连不通时交接会失败，由自检（`PD_HANDOFF_FAILED`）与降级率反映出来，必要时整组重启。
- **重启**：部署详情页头的"重启"（`POST /api/v1/deployments/{id}/restart`，所有部署形态通用）按当前解析重建全部实例，每个实例的
  重试次数重新计算。默认逐个替换（其余实例照常服务，一个实例恢复后才换下一个）；选"全部一起重启"时停掉全部实例后一起重建，期间服务中断。
- **监控**：服务页的 PD 卡片给出：
  - PD 有效率（decode 收到的提示中经传输得到的比例）：明显低于 100% 说明分离并没有真正发生（交接失败后降级到 decode 直算）；
  - 降级率与主要原因；
  - prefill 阶段平均与 P95（网关等 prefill 实例的时间）、prefill 实例的排队、平均提示长度；
  - KV 传输：平均 / P95 传输时间、平均大小、吞吐、失败与过期次数，取自 vLLM NIXL 连接器的 `vllm:nixl_*` 指标，按次传输统计、
    不对应单个请求。失败是 decode 拉取 KV 失败的次数；过期是 prefill 实例算好的 KV 在租约（vLLM 0.30 默认 30 秒，decode 收到请求后
    持续续约）到期前没被拉走、只好释放的请求数，说明 prefill 之后请求没有到达
    decode（客户端断开、交给 decode 失败）或 decode 实例出了故障。`vllm-ascend-mooncake` 的连接器不导出这些指标，卡片显示「引擎不提供」；单次传输时间只在 decode 实例日志里
    （`KV cache transfer for request … took … ms`）。
- **请求**：请求视图在「网关」之后单列「PD Prefill」段；端到端延迟拆解里同一段直接叫「Prefill」。decode 实例自报的排队显示为
  「KV 传输·排队」（vLLM 与 vllm-ascend 都把等远端 KV 的时间算在排队里），自报的 Prefill 显示为「首 token 重算」；延迟拆解里
  它只占端到端平均的 1% 或更少时不画出。请求的缓存命中（以及返回给客户端的 `cached_tokens`）取 prefill 实例的值，因为 decode 把拉来的 KV 也算作命中，见 [OpenAI 兼容网关](../reference/openai-gateway.md#用量usage)。请求详情的 PD 一节给出
  prefill 实例、prefill 阶段、该实例自报的排队与 Prefill、降级原因；还在 prefill 阶段的请求也出现在在途表里。字段定义见
  [请求时间线与阶段拆分](../reference/request-timeline.md#6-pd-分离部署)；导出的 OTel trace 里 prefill 阶段是 `pd.prefill` 子 span。
- **评测**：默认经网关测端到端；指定某个实例时直接测该实例，结果只代表一个普通副本。

## 排查

- 降级率高：看网关计数的原因（`prefill_unreachable`、`prefill_5xx`、`prefill_no_kv_params` 等）与 prefill 实例日志。
  `prefill_no_kv_params` 通常说明 prefill 的 KV 连接器没有生效。
- 已知缺陷：引擎进程在实例就绪后退出时，平台暂时不会发现，实例仍显示运行中；PD 下表现为网关持续降级。
  重启该部署或调整角色可以替换它。
