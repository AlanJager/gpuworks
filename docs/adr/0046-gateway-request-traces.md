# ADR 0046: 网关为每个请求建立 W3C trace context，trace id 写进请求记录，逐请求记录按 OTLP 导出

日期：2026-10-15 · 状态：已接受（决策 3 中顺序交接的 prefill 请求改以 `pd.prefill` span 为父，见 [ADR 0047](0047-pd-request-breakdown.md)）

依据：[请求级 trace 计划](../../artifacts/archive/2026-10-15-otel-export/plan.md)。

## 背景

用户要把平台的请求级数据接到自己的监控：上游网关（Higress、Kong、LiteLLM 等）已经在打 trace，希望平台网关这一段接在同一条 trace 里；也希望在外部追踪系统和平台的请求视图之间来回跳。

可选的数据源有两个：

- **引擎自己的 OTel**（vLLM `--otlp-traces-endpoint`）：只覆盖 vLLM；span 里有排队、Prefill、Decode，但没有缓存命中、思考 token、客户端断开、调用方；引擎容器要能连到用户的 Collector，而节点通常只能往外连 server；还要镜像里带 OTel SDK。
- **网关的逐请求记录**（`uxrecord.Request`，[ADR 0042](0042-gateway-request-records.md)）：每个引擎都有，字段是 vLLM span 的超集（引擎自报的排队 / Prefill / Decode 也在里面），只在 server 上。

## 决策

1. **请求身份在网关一处确立**：`proxy.serveSelectedTarget`（所有转发请求、含健康探测都经过）为请求定下请求 ID 与网关 span（`internal/server/tracecontext`）。请求带了合法的 `traceparent`（W3C 处理规则：只一个、版本非 `ff`、小写十六进制、两个 id 都非全零）就接上调用方的 trace，以调用方 span 为父、沿用其 sampled 位与 `tracestate`；否则新起一个 trace。
2. **网关新起的 trace id 前 6 字节是毫秒时间戳**，后 10 字节随机，置 Trace Context Level 2 的 random 位（右 7 字节随机）。这让请求存储不建索引也能按 trace id 找到请求。
3. **转给引擎的请求带网关 span 作父**：`traceparent` 换成网关 span，`tracestate` 原样；重试与 PD 的 prefill / decode 两段都是网关 span 的子请求。用户自己给 vLLM 加 `--otlp-traces-endpoint` 时，引擎 span 自动挂在网关 span 下；平台不接管这个参数。
4. **请求记录带 trace id，存储不为它建索引**：`ux.db` 请求表加 `trace_id BLOB NOT NULL DEFAULT x''`，打开时为缺这一列的日分区补上（与补 `lat_` 表同一机制），不改 `user_version`，旧版本仍能读写同一文件（回滚安全）。查找（`GET /api/v1/monitor/requests/lookup`）按时间窗进行：先从当天的计数表列出窗口内有流量的部署，再沿 `(deployment_id, ts, seq)` 索引扫描、逐行比较 id。窗口最长 24 小时；网关新起的 trace id 自带时间，不给窗口也能直接定位；其余默认查最近 1 小时。请求 ID 同样查找。
5. **导出源是请求记录**：`internal/server/traceexport` 在请求结束后，只从这一条记录构造一个 SERVER span（时间线是 span event，网关侧阶段取 `uxrecord.SegmentsOf`，与请求视图同一套拆分），按 OTLP/HTTP JSON + gzip 批量推给管理员配置的 Collector。不引 OTel SDK 与 gRPC。属性名按三条规则取：OTel GenAI / HTTP 语义约定有的用约定的名字（含采样参数 `gen_ai.request.max_tokens`、`temperature`、`top_p`、`choice.count` 与 `gen_ai.request.stream`）；约定没有而 vLLM 的 `llm_request` span 有的，用 vLLM 的名字与单位（`gen_ai.latency.e2e`、`time_in_queue`、`time_in_model_prefill`、`time_in_model_decode`、`time_in_model_inference`，秒，值为引擎自报原值）；两边都没有的用 `gpuworks.*`。采样参数由网关对请求体的同一遍扫描读出，只在内存记录里，不落库。
6. **采样**：调用方 trace 沿用调用方的 sampled 位（ParentBased）；网关新起的 trace 按 trace id 低 56 位与比例比较。导出关闭时不采样。`ux.db` 不受采样影响。
7. **导出不阻塞转发**：有界队列，满了丢弃计数；单协程批量发送，429/502/503/504 与连接失败按 `Retry-After` 或指数退避重试 3 次，其余丢弃；状态（已导出、丢弃、失败、最近错误）在设置页显示。
8. **配置只有一处**：平台设置 `telemetry_export`（`store.TelemetryExport`），header 值用 secretbox 加密；只经 `/api/v1/telemetry/export` 读写（通用设置接口不列出、不接受），保存后立即生效，server 启动时应用。不读 `OTEL_*` 环境变量，避免两处来源。

## 取舍

- **事后构造 span，不在热路径埋 tracer**：网关每请求只多一次 `traceparent` 解析与 span id 生成（约 0.3 µs、1 次分配）；span 的每个值都来自记录，导出内容与请求视图不会对不上。代价是只有一个网关 span，阶段以属性和事件表达，而不是子 span。
- **trace id 不建索引**（2026-10-15 基准，100 万行）：随机 trace id 的索引让每行写入从 59 µs 升到 250 µs（每次插入弄脏一个不同的索引页），加大页缓存无效；不建索引只多 12%。代价是查找必须给时间窗，抽样掉的正常请求查不到（异常请求全留；全量在外部追踪系统里）。
- **不升 schema 版本**：新增列有默认值，旧版本按列名写入不受影响，回滚不必挪走 `ux.db`。
- **不用引擎 OTel 当数据源**：覆盖面窄、要求节点能连 Collector、字段更少；保留为用户自选的补充。
- **只做 traces**：聚合指标的导出（Prometheus 抓取端点、OTLP metrics）另议。

## 后果

- 请求视图与查找接口的每条请求带 `trace_id` 与 `deployment_id`；在途请求同样带 `trace_id`。
- 上游网关传 `traceparent` 后，外部追踪系统里平台网关 span 是上游 span 的子 span；两边靠 trace id 对应。请求 ID 仍由 server 自己分配（访问日志的 `rN`，客户端的 `X-Request-ID` 不会替换它），span 的 `gpuworks.request.id` 与响应头 `X-Request-ID` 是同一个值。
- 设置页可以配置 Collector 地址、header、采样比例与 trace 链接模板，并发测试 span。
- 文档：[导出请求 trace 到 OpenTelemetry](../how-to/export-request-traces.md)、[OpenAI 兼容网关](../reference/openai-gateway.md#trace-context)、[API 参考](../reference/api.md)。
