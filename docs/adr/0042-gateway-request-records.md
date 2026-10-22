# ADR 0042: 网关为流式请求向 vLLM / SGLang 要 usage 并剥离多出的块，逐请求记录写独立的 ux.db

日期：2026-10-12 · 状态：已接受（修订推理指标计划 D4「网关不注入 `stream_options`、不改写任何请求」）

依据：[请求级体感可观测计划](../../artifacts/work/2026-10-12-request-ux-observability/plan.md)与[调研](../../artifacts/research/2026-10-11-llm-ux-observability/README.md)。

## 背景

只看 TTFT p95 判断不了用户体感：同样 100 秒的首字，可能是十万 token 的长输入、可能没命中前缀缓存、也可能在排队。要把它说清楚，每个请求至少要知道输入多长、命中了多少缓存、输出多长、思考了多久，以及排队、Prefill、Decode 各花了多少时间。

这些事实只有引擎知道，而且只在回复里给：

- vLLM 与 SGLang 在流式请求里只有客户端带了 `stream_options.include_usage` 才发 usage（含 `prompt_tokens_details.cached_tokens`）。多数客户端不带，于是网关读不到这些请求的 token 数，推理指标计划里的 `usage_missing_total` 记的就是它们。vLLM 0.30、0.31 与 main 的 OTel span 也不带缓存命中数，抓 Prometheus 只有聚合值。
- vLLM 0.25.0 起的 `--enable-per-request-metrics` 把该请求的排队、首 token、生成耗时写进回复最后一个 usage 块（`metrics` 对象）；TensorFold、Strata 在最后一个事件里自带 `tensorfold`、`timings` 对象，不需要客户端要求。

推理指标计划的 D4 曾决定网关不注入 `stream_options`、不改写任何请求。

## 决策

1. **只对 vLLM / SGLang 的流式请求注入 usage 请求**：`openai.chat` 操作的请求体顶层是流式（`"stream": true`）且没有 `stream_options` 时，网关在对象开头插入 `"stream_options":{"include_usage":true}`，其余字节不动。客户端自己写了 `stream_options`（不论内容）时原样转发。callable 引擎不注入：它们可能拒绝不认识的字段，平台内置的 TensorFold、Strata 本来就总带 usage。
2. **剥离因此多出的块，客户端收到的字节与不注入时一致**：注入过的回复按 SSE 行过滤，丢掉 `choices` 为空数组、`usage` 是对象的那一行及其后的空行；SGLang 的扩展块也可能 `choices` 为空，有的版本还在每个块带 `"usage": null`，所以只有 `usage` 是对象才算。vLLM 在被要求 usage 时把 `system_fingerprint` 从结束块挪到 usage 块，网关把结束块暂存到下一个事件，若下一个是 usage 块就把指纹放回结束块。其余字节原样、完整一行就放行；超过 16 KiB 的行直接透传不暂存。vLLM 0.30 与 SGLang 0.5.21 的真实抓包对证明了逐字节一致（`internal/server/proxy/testdata/vllm-0.30-*.sse`、`sglang-0.5.21-*.sse`），gpu3 上经网关的 SGLang 请求也与引擎直接返回的流一致。
3. **引擎写在回复里的逐请求统计按声明读取**：词表在 `internal/inferencemetrics`（`queue_seconds`、`prefill_seconds`、`decode_seconds`），vLLM 内置读 `metrics.*`，其他引擎由模板 `api.request_stats` 声明。vLLM 新部署默认带 `--enable-per-request-metrics`（参数字典 `enable_request_metrics`，部署表单上可见可关），只对镜像目录或镜像 tag 表明为 vLLM 0.25.0 及以上的镜像生效；`metrics` 字段原样留给客户端，因为这个启动参数是部署时用户确认过的，与 `--enable-prompt-tokens-details` 相同。
4. **逐请求记录写独立的 `ux.db`**：网关为每个转发的 `/v1` 与 Playground 请求产出一条 `uxrecord.Request`（身份、时间线、usage、引擎统计、结局），经非阻塞队列交给 `uxstore`。`ux.db` 是数据目录下单独的 SQLite 文件，按 UTC 日分区（请求日志、分布网格、计数三张表），整表删除过期分区：保留 7 天，超过 2 GB 从最老的一天删起。分布网格与计数按全部请求累加；请求日志每秒超过 300 行时，异常请求（非 2xx、断开、截断、首个输出 ≥ 10 秒）全留，正常请求按步长抽样并在 `weight` 记它代表几个请求。队列满时丢弃并计数，绝不阻塞转发。
5. **进行中的请求只在内存里**：按部署分片的在途表，事件只做原子计数；排队与 Prefill 在请求结束前合并为「等首字」，不显示 Prefill 进度。

## 取舍

- **改写请求体 vs 保持字节不动**：放开 D4 是为了让 vLLM / SGLang 的大多数流式请求都有 usage，分布图与非缓存输入 × 首字图才有覆盖率。代价是网关多一次请求体扫描与插入拷贝、回复按行过滤；剥离保证客户端看到的回复不变，所以对客户端仍然透明。客户端自己要 usage 的请求一个字节都不改。
- **注入范围只限 vLLM / SGLang**：别的引擎是否接受 `stream_options` 无从保证，宁可缺数据也不让请求失败。
- **独立文件而不是主库或 metrics.db**：请求日志写入量随流量线性增长，与主库的事务、metrics.db 的时序写入互不干扰；按日整表删除让保留和容量上限不需要逐行删除。抽样带权重，计数与网格不抽样，所以分布和覆盖率仍然是全量的。
- **网关测不出排队与 Prefill 的分界**：进行中只给「等首字」的下界；拆开靠请求结束后引擎自报的统计，没有的引擎就不拆。

## 后果

- vLLM / SGLang 部署的流式请求不再大量计入 `usage_missing_total`；客户端自己要 usage 的行为不变。
- 网关每请求的额外开销：一次请求体字节扫描、注入时一次拷贝、每个事件一次字节匹配与原子计数、结束时解析末尾 4 KiB；监控 API 只读单个部署的在途表与 `ux.db` 的索引范围。
- 运行中的 vLLM 部署不会被要求重启：存量部署的启动配置里没有这个参数，重建也不会补上；新部署才默认带。
- 文档：[推理指标词表的逐请求统计](../reference/inference-metrics.md#逐请求统计)、[`api.request_stats` 语法](../reference/inference-catalog-spec.md#apirequest_stats回复里的逐请求统计)、[查看指标与检索日志](../how-to/monitor-and-logs.md#2-看请求视图与请求分布)。
