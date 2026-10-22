# ADR 0047: PD 请求的 prefill 阶段由网关实测并单列，KV 传输只给聚合统计

日期：2026-10-15 · 状态：已接受（修订 [ADR 0046](0046-gateway-request-traces.md) 决策 3：顺序交接的 prefill 请求以 `pd.prefill` span 为父）

依据：[PD 请求阶段拆分计划](../../artifacts/work/2026-10-15-pd-request-breakdown/plan.md)。

## 背景

PD 分离部署（[ADR 0028](0028-pd-disaggregated-deployment.md)）的顺序交接里，网关先把请求（`max_tokens=1`）发给 prefill 实例、拿到 KV 参数后才交给 decode 实例。原来的拆分把这段等待算进「网关」：gpu3 上 vllm-nixl 部署的「网关」段几百毫秒，其中网关自身约 21 ms，其余是等 prefill 实例。decode 实例自报的数在 PD 下也变了含义：vLLM 调度器在等远端 KV（`WAITING_FOR_REMOTE_KVS`）期间把请求留在等待队列，自报的排队包含 KV 传输；自报的 Prefill 只是重算最后一个提示 token。

能拿到的数据（2026-10-15 核实）：

- 单个请求的 KV 传输时间，两个引擎都不导出。vLLM 的 NIXL 连接器只有聚合指标 `vllm:nixl_*`（llm-d 的 PD 监控也只用这些，并注明不对应单个请求）；vllm-ascend 0.27.1rc1 的 `MooncakeConnectorV1` 没有任何指标，只在 decode 日志写 `KV cache transfer for request <id> took X ms`。
- prefill 实例的一 token 回复里同样带逐请求统计（`--enable-per-request-metrics`），读法与 decode 回复相同。
- vllm-ascend 用的是 vLLM 的调度器，KV 等待同样计入排队。

## 决策

1. **prefill 阶段由网关实测，作为请求记录的事实**：`uxrecord.Request.PD`（`proxy/pd_stage.go` 记下）保存 prefill 实例、网关等它的时长（含失败后换实例）、prefill 实例自报的排队与 Prefill、降级原因，落 `ux.db` 请求表的 5 个追加列，汇总表 `lat_` 加 `pd_prefill_ms`。与 `trace_id` 同一机制：带默认值、打开时补列、不升 `user_version`，回滚安全。并发交接（`sglang-nixl`）网关不等 prefill，等待记未知，只记实例与降级。
2. **拆分仍只在 `uxrecord.SegmentsOf`**：新段 `pd_prefill = min(pd.prefill_ms, dispatch)` 紧随 `gateway`，`gateway = dispatch − pd_prefill`；各段之和仍等于端到端，延迟拆解、请求色条与 OTel `gpuworks.phase.*` 自动一致。
3. **decode 自报的数不改值、只改名**：PD 部署的界面把 `queue` 叫「KV 传输·排队」、`prefill` 叫「首 token 重算」；API 与存储里仍是同一个字段。不从排队里扣出一段「KV 传输」——没有逐请求的数可扣。
4. **KV 传输只给聚合统计**：服务页 PD 卡片从 decode 实例的 `nixl_*` 采集值算平均 / P95 传输时间、平均大小、吞吐与失败，过期取 prefill 实例的计数（vLLM 只在 prefill 侧记 `nixl_num_kv_expired_reqs`）。某个 PD 模式的引擎导不导出这些指标只由 `pdmode.Mode.TransferBytesMetric` 判断，不导出时 API 给 `kv_transfer.exposed=false`，界面写「引擎不提供」而不是一排破折号。不解析 vllm-ascend 的日志，也不给它打指标补丁。
5. **在途表含 prefill 阶段中的请求**：顺序交接从开始等 prefill 起就登记在途（健康探测除外），此时 `dispatch_ms` 未知、`pd_prefill` 段走到此刻；prefill 失败且按 `fail` 返回 503 的请求随即撤下。
6. **缓存命中取 prefill 实例的值，客户端也一样**（2026-10-16 补充）：decode 实例的 usage 把拉来的 KV 算作 `cached_tokens`，总等于整个提示。顺序交接时网关从 prefill 实例回复的 usage 读出它的命中数（与 decode 回复同一读法 `cacheFigure`），在 decode 回复里原地替换（`proxy/cached_rewrite.go`：数字变短补空格，长度不变）；改写在网关的回复观察器之下，客户端、请求记录、网关计数与 OTel 读到同一个数。PD 请求的 Prefill 速度用 prefill 实例自报的 Prefill（`uxrecord.Request.UncachedComputeMS`）。降级、并发交接、prefill 回复没有这个数时不改。
7. **OTel**：顺序交接的请求多一个 CLIENT 子 span `pd.prefill`（span id 在阶段开始时生成，只在内存记录里），发给 prefill 实例的 `traceparent` 以它为父；属性是 prefill 实例、其自报的 `gen_ai.latency.time_in_queue` / `time_in_model_prefill`（与父 span 同名、各自的引擎）、降级原因。父 span 加 `gpuworks.pd.prefill_instance`、`gpuworks.pd.fallback`。并发交接没有子 span，prefill 请求仍以网关 span 为父。

## 取舍

- **新增持久化事实**：prefill 实例、等待、自报值与降级原因无法从其他数据推出（网关计数只有聚合），而历史请求的详情要看，所以落库；span id 不落库（只有导出用，导出在请求结束时已完成）。
- **不做逐请求 KV 传输**：要么解析引擎日志（只有 vllm-ascend 有、格式随版本变、要把 decode 日志接到网关），要么给引擎打补丁，都与「引擎自报的统一读法」相冲突；聚合值足以判断链路是否是瓶颈。
- **改写客户端看到的 usage**：与直连 decode 实例时不同，是有意为之。按 OpenAI 的语义，`cached_tokens` 是不必重新计算的提示 token，客户端用它看缓存效果、算缓存折扣；decode 的数在 PD 下恒为 100%，会误导。不加开关：原值没有可用的含义。
- **改名不改值**：保持一个字段一个含义的存储，PD 的解释只在展示层；代价是 API 使用者要看部署是否是 PD 才知道 `engine_queue_ms` 含 KV 等待，已写进参考文档。

## 后果

- 请求视图、延迟拆解、请求详情（PD 一节）与导出的 trace 都能分开网关自身、prefill 阶段与 decode 的 KV 等待。
- API：请求的 `pd` 对象、`segments` 的 `pd_prefill`、服务快照 `pd.prefill_stage_p95_ms` 与 `pd.kv_transfer`，见 [API 参考](../reference/api.md)。
- 文档：[请求时间线与阶段拆分](../reference/request-timeline.md#6-pd-分离部署)、[PD 分离部署](../how-to/pd-disaggregation.md#运行中)、[导出请求 trace](../how-to/export-request-traces.md#附录span-字段)。
