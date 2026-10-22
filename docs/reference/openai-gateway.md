# OpenAI 兼容网关

server 的 /v1/* 使用 API key 鉴权；请求体中的 model 选择 served model，server 再解析部署、
实例和节点，通过 agent tunnel 转发到本地端口。管理 API 使用 JWT，不能用管理 JWT 代替网关 key。

## 支持的操作

| 操作 | 默认 endpoint | 编码/模式 |
|---|---|---|
| chat | /v1/chat/completions | JSON |
| text completion | /v1/completions | JSON；可流式。提示词原样交给引擎，不套聊天模板，用于基座模型和按固定格式驱动的任务模型 |
| embeddings | /v1/embeddings | JSON |
| rerank | /v1/rerank（兼容 /rerank） | JSON |
| audio transcription | /v1/audio/transcriptions | multipart |
| audio speech | /v1/audio/speech | JSON |
| image generation | /v1/images/generations | JSON 或 multipart |
| video generation | /v1/videos | JSON 或 multipart；可 async |
| realtime | /v1/realtime | WebSocket |

每个推理 spec 的 API contract 声明该部署可用的操作和 endpoint。控制台试验场（`/api/v1/playground/v1/*`）按 contract 限定，不在其中的路径返回 404；公网 `/v1` 不做 contract 检查，只按 `model` 选副本并转发，引擎不支持的路径由引擎自己返回（通常是 404）。
视频异步任务还提供 GET /v1/videos/{id}、GET /v1/videos/{id}/content 和 DELETE /v1/videos/{id}。
多副本部署默认使用 least-request：网关在内存候选表中按本机在途请求数选择副本，并把新鲜的引擎排队数/KV 使用率作为软惩罚。连接级失败会在请求仍可重放时换副本重试；已经收到上游响应（包括 5xx）不会重试。实例休眠、启动中或 rank 非 0 不可服务时返回 503，并保留重试语义。

部署可以通过管理 API `PUT /api/v1/deployments/{id}/routing` 选择 `least_request`、`prefix_affinity` 或 `failover`。前缀亲和优先读取 `X-GPUWorks-Affinity-Key`，也支持从 chat messages、prompt 或 input 提取稳定 key；该请求头不会转发给引擎。

## 分词（tokenize）

`POST /v1/tokenize` 与 `POST /v1/detokenize` 按请求体的 `model` 选部署与副本，和其他 `/v1` 请求一样走 API key 鉴权、模型名改写、休眠/启动中的 503 与连接级重试，但转给引擎的根路径 `/tokenize`、`/detokenize`（vLLM 只在根路径提供，SGLang 根路径与 `/v1` 都有）。请求与回复原样转发，形状是引擎自己的，例如 vLLM：

- `/tokenize` 收 `prompt`，或 `messages`（可带 `add_generation_prompt`，缺省 `true`、`chat_template_kwargs`），另有 `add_special_tokens`、`return_token_strs`；回复 `{"count", "max_model_len", "tokens", "token_strs"}`，`token_strs` 不要时为 `null`。
- `/detokenize` 收 `tokens`（整数数组），回复 `{"prompt": "..."}`。
- 引擎的校验错误（400）与模型不存在（404，`{"error": {"message", "type": "NotFoundError", "param": "model", "code": 404}}`）原样返回。

分词不是推理流量：不计入网关的请求数、耗时与在途数，不留请求记录，不更新部署的最近请求时间，也不导出 trace。只接受 POST，其他方法网关回 405（带 `Allow: POST`）。根路径 `/tokenize` 归控制台，网关不接管。

每个部署可以关掉分词转发：创建时 `tokenize_enabled: false`，或 `PUT /api/v1/deployments/{id}/tokenize`（见 [API 参考](api.md)）；默认开启。关掉后网关直接回 404（`deployment <model> does not expose POST /v1/tokenize: tokenize is turned off for it`），不联系引擎。开关随网关路由表生效，不重启实例。vLLM、SGLang 以外的引擎未必提供分词接口，届时请求收到引擎自己的错误。

## Trace context

网关为每个转发的请求建立一个 W3C trace context（[ADR 0046](../adr/0046-gateway-request-traces.md)）：

- 请求带合法的 `traceparent` 时，网关接上调用方的 trace：网关自己的 span 以调用方 span 为父，沿用调用方的采样决定与 `tracestate`。上游网关（Higress、Kong、LiteLLM 等）开启 W3C 传播后，平台这一段就接在它的 trace 里。
- 没带或不合法时，网关新起一个 trace；trace id 的前 6 字节是网关收到请求的毫秒时间（右 7 字节随机，置 Trace Context Level 2 的 random 位）。
- 转给引擎（含重试、PD 的 prefill 与 decode）的请求带 `traceparent`，父是网关 span（PD 顺序交接发给 prefill 实例的请求以网关的 `pd.prefill` 子 span 为父，见 [ADR 0047](../adr/0047-pd-request-breakdown.md)），`tracestate` 原样转发。引擎自己开了 OTel（例如 vLLM 的 `--otlp-traces-endpoint`）时，它的 span 挂在网关 span 下。
- 响应头 `X-Request-ID` 是 server 分配的请求 ID，与 trace id 一起记在请求记录里；客户端发来的 `X-Request-ID` 不会替换它，跨系统对应请用 trace id。
- 把这些 span 导出到外部追踪系统见[导出请求 trace 到 OpenTelemetry](../how-to/export-request-traces.md)；在控制台按 trace id 或请求 ID 查找请求见[查看指标与检索日志](../how-to/monitor-and-logs.md#2-看请求视图与请求分布)。

## 用量（usage）

回复里的 `usage` 原样来自引擎，只有两处不同：

- 客户端没要求流式 usage（`stream_options.include_usage`）时，网关为自己的统计向引擎要了一份，并从流里删掉它带来的那个 usage 事件，客户端收到的流与没要时一样。
- PD 分离部署顺序交接（`vllm-nixl`、`vllm-ascend-mooncake`）的请求，`prompt_tokens_details.cached_tokens` 是 **prefill 实例**回复里的值：decode 实例把从 prefill 实例拉来的 KV 也算作缓存命中，它报的数总等于整个提示；真正省下计算的是 prefill 实例的前缀缓存命中。网关原地替换这个数（不足的位数补空格，`Content-Length` 不变），usage 的其他字段仍是 decode 实例写的。prefill 实例的回复不带这个数、请求降级由 decode 单独完成、或 SGLang 并发交接时，保留 decode 实例的值。平台的请求记录、网关计数与导出的 trace 用的是同一个数。

## Playground

模型列表只展示已部署且声明了推理 API 操作的模型；全局设置中的外部模型供 AI 助手使用，不自动加入试验场。

已部署模型可从管理 JWT 保护的 /api/v1/playground/v1/* 调用；外部模型名形如
external:<config-id>:<model>，仅允许 chat completions。外部 endpoint key 在 server 端加密解密
后转发，不写入浏览器或实例环境。

## 差异与排查

网关不模拟 OpenAI 的全部产品接口，具体能力以 spec contract 和
GET /api/v1/playground/openapi.json 为准。收到 401 先检查 Authorization: Bearer <key>；404 检查
model 名称（试验场还要看 contract）；503 检查实例状态、sleep/wake 和节点在线性；502 查看 tunnel 和引擎日志。
流式 SSE 与二进制（例如 WAV）按原始 content type 转发，不截断响应。
