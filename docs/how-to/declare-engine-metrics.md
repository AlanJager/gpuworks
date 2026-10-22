# 为推理模板声明引擎指标

**适用场景**：你写了一个推理模板（尤其是 `engine: callable` 的自研或第三方引擎），部署后部署抽屉、实例页和服务监控页的推理面板没有数据，或者想让引擎自己的 TTFT、KV 使用率、投机接受率进入平台面板。

**前置条件**：引擎在容器里提供 Prometheus 文本格式的指标端点；你能导入或编辑推理模板（见[推理目录 model.yaml 语法参考](../reference/inference-catalog-spec.md)）。

## 先判断要不要声明

agent 每 10 秒抓一次每个运行中实例的指标端点（`127.0.0.1:<实例端口><endpoint>`），按模板解析出的映射把引擎指标转成平台指标（[推理指标词表](../reference/inference-metrics.md)）。没有声明 `api.metrics` 时：

| 模板引擎 | 默认行为 | 需要声明吗 |
|---|---|---|
| `vllm` / `sglang` | 用平台内置的 vLLM / SGLang 映射 | 不需要；要追加或改几条时写 `map` 即可 |
| `callable`，引擎导出 `vllm:` 或 `sglang:` 开头的指标（vLLM / SGLang 的改版、包了一层 SGLang 的适配器） | 抓 `/metrics`，按抓到的前缀自动套用 vLLM 或 SGLang 的映射 | 一般不需要 |
| `callable`，引擎用自己的名字（如 TensorFold 的 `tensorfold:`） | 抓 `/metrics`，但没有任何名字能对上，不出引擎指标 | 需要 |
| 引擎的指标端点返回 JSON（如 Strata 0.1.38 的 `/metrics`） | 按 Prometheus 文本解析不出任何一行 | 需要声明 `format: json`，见下文 |
| 引擎没有任何指标端点 | 抓取失败被跳过 | 声明也没用；延迟、token 等由网关统计 |

`callable` 模板没有声明 `api.metrics` 时，导入（以及 `POST /api/v1/inference-templates/evaluate` 预检）会返回警告 `CATALOG_CALLABLE_METRICS_UNDECLARED`，界面在导入或保存后列出它；警告不阻止导入，引擎确实没有指标端点或用的就是 vLLM / SGLang 的指标名时可以忽略（见[导入前检查与警告](../reference/inference-catalog-spec.md#导入前检查与警告)）。

## 1. 看引擎的原始指标

在实例所在的节点上直接请求实例端口（端口见实例详情；agent 抓取用的就是这个地址）：

```bash
curl -s http://127.0.0.1:<实例端口>/metrics | grep -v '^#' | sort | head -100
```

runveil 节点也一样在宿主机上请求；容器里有 curl 时也可以 `gpuworks-agent runveil exec <容器名前缀> curl -s http://127.0.0.1:<实例端口>/metrics`（见 [CLI 参考](../reference/cli.md#gpuworks-agent-runveil)）。

看一下响应的 `Content-Type`：`application/json` 的是 JSON 端点，按下文 [JSON 端点](#json-端点) 写映射。Prometheus 文本的，对着输出确认三件事：族名（`tensorfold:requests_running`）、类型（`# TYPE` 行；直方图会有 `_bucket` / `_sum` / `_count` 三组行）、标签（同一族是否按 `tier`、`state` 等标签分成多行）。

## 2. 写映射

在 spec 的 `api` 下加 `metrics`，键是引擎指标，值是平台指标的**短名**：

```yaml
api:
  operations:
  - type: openai.chat
  metrics:
    endpoint: /metrics       # 可选，默认 /metrics
    map:
      'tensorfold:requests_running': requests_running
      'tensorfold:time_to_first_token_seconds': ttft_seconds
      'engine:kv_used_percent': {to: gpu_cache_usage_percent, scale: 0.01}
```

规则：

- **目标写短名**（`ttft_seconds`），不写存储名 `gpuworks_infer_ttft_seconds`；短名必须在[词表](../reference/inference-metrics.md)里。
- **直方图用族名映射**：写 `tensorfold:time_to_first_token_seconds`，不写 `..._bucket`；agent 会把 `_bucket`、`_sum`、`_count` 一起转过去。直方图不能带标签选择器，不能换算；也不要再单独映射同一族的 `_count` 等行。
- **按标签挑一行**：键可以带选择器，例如 `'vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs"}': kv_disk_hits_total`，只取标签匹配的行；值必须加引号。
- **其他标签不保留**：存下来的序列只带平台标签（实例、部署、引擎、节点）。计数器族有多行（例如按 `model_name` 分开）时相加；gauge 族有多行时只取第一行，所以按状态等标签分行的 gauge 要用选择器挑出你要的那一行。
- **换算**：长写法 `{to: <短名>, scale: <系数>}`，对计数器和 gauge 生效，例如 0～100 的百分比转 0～1 的比例写 `scale: 0.01`、以千 token 计的计数转成 token 写 `scale: 1000`。
- **继承**：`inherit: vllm` 或 `inherit: sglang` 先套用该引擎的整套内置映射，再应用 `map`；同一个键以 `map` 为准（模板优先）。`vllm` / `sglang` 引擎的模板不写 `inherit` 也会继承自身的映射。
- 写了 `map` 或 `inherit` 之后，callable 的自动识别就不再进行。

只报 KV 池容量和空闲量、不报使用率的引擎，映射到 `kv_capacity_tokens` 和 `kv_free_tokens` 即可，KV 使用率由平台派生（见词表的[派生指标](../reference/inference-metrics.md#派生指标)）。

## 3. 导入并修正校验错误

导入 YAML、上传目录包或在模板编辑器里保存时，`api.metrics` 和其余字段一起校验，有错整份拒绝。模板编辑器里输入目标名时有补全。

| 错误码 | 原因 |
|---|---|
| `CATALOG_METRICS_ENDPOINT_INVALID` | `endpoint` 不是以 `/` 开头的路径 |
| `CATALOG_METRICS_INHERIT_UNKNOWN` | `inherit` 不是 `vllm` 或 `sglang` |
| `CATALOG_METRICS_SOURCE_INVALID` | 键不是合法的指标族名，或选择器写法不对（值要加双引号） |
| `CATALOG_METRICS_UNKNOWN_METRIC` | 目标短名不在词表里（拼错，或写成了 `gpuworks_infer_` 开头的存储名） |
| `CATALOG_METRICS_HISTOGRAM_SELECTOR` | 直方图目标的键带了标签选择器 |
| `CATALOG_METRICS_HISTOGRAM_SCALE` | 直方图目标写了换算 |
| `CATALOG_METRICS_NEGATIVE_SCALE` | 换算系数为负 |
| `CATALOG_METRICS_SHADOWED_HISTOGRAM` | 映射了某个已映射直方图族的 `_bucket` / `_sum` / `_count` 行 |
| `CATALOG_METRICS_DUPLICATE_SOURCE` | 同一个键（去掉首尾空白后）写了两次 |
| `CATALOG_METRICS_FORMAT_INVALID` | `format` 不是 `prometheus` 或 `json` |
| `CATALOG_METRICS_JSON_INHERIT` | `format: json` 时写了 `inherit` |
| `CATALOG_METRICS_JSON_PATH_INVALID` | `format: json` 时键不是用点连接的键路径 |
| `CATALOG_METRICS_JSON_HISTOGRAM` | `format: json` 时映射到了直方图（JSON 端点只能填计数器和 gauge） |
| `CATALOG_METRICS_JSON_COMPARE_COUNTER` | `format: json` 时把比较式（`路径!=词`）映射到了计数器 |

## 4. 确认生效

- **新部署**按当前模板的映射采集。
- **已经在运行的部署不用重启、不用重新部署**：模板保存后，server 重新解析引用该档位的运行中实例的映射，原地推给 agent，下一轮抓取（10 秒内）生效。server 升级带来的内置模板映射变化同样如此。节点当时离线的，重连后拿到。
- 在部署抽屉或服务监控页看推理面板；或者在指标浏览器里查 `gpuworks_infer_requests_running{deployment="<部署 ID>"}`。

旧版本 agent 只认简单映射：带换算的条目、派生的 KV 使用率和 callable 的自动识别要等节点 agent 升级后才生效。

## 示例

**TensorFold**（内置 GLM-5.3-Flash TensorFold 模板，名字取自 TensorFold 0.6.0 的真实 `/metrics`）：

```yaml
metrics:
  map:
    'tensorfold:requests_running': requests_running
    'tensorfold:requests_waiting': requests_waiting
    'tensorfold_health:requests_total': requests_success_total
    'tensorfold:prompt_tokens_total': prompt_tokens_total
    'tensorfold_health:completion_tokens_total': generation_tokens_total
    'tensorfold_health:cached_tokens_total': prompt_tokens_cache_hit_total
    'tensorfold:mtp_drafted_total': spec_draft_tokens_total
    'tensorfold:mtp_accepted_total': spec_accepted_tokens_total
    'tensorfold:time_to_first_token_seconds': ttft_seconds
    'tensorfold:request_latency_seconds': e2e_seconds
    'tensorfold_health:pool_tokens': kv_capacity_tokens
    'tensorfold_health:pool_free_tokens': kv_free_tokens
```

**包了一层 SGLang、但改了指标前缀的适配器**：继承 SGLang 的映射，再把改名的几条补上。

```yaml
metrics:
  inherit: sglang
  map:
    'adapter:num_running_reqs': requests_running
    'adapter:num_queue_reqs': requests_waiting
```

**指标端点不在 `/metrics`、KV 使用率按百分比报告的自研引擎**：

```yaml
metrics:
  endpoint: /stats/prometheus
  map:
    'myengine:active_requests': requests_running
    'myengine:pending_requests': requests_waiting
    'myengine:output_tokens_total': generation_tokens_total
    'myengine:kv_used_percent': {to: gpu_cache_usage_percent, scale: 0.01}
```

## JSON 端点

有的引擎把统计放在一个 JSON 文档里返回，而不是 Prometheus 文本，例如 Strata 0.1.38 的 `/metrics`：

```json
{"live": {"state": "generating", "queued": 0, "prompt_tokens": 50, "generated": 1971},
 "totals": {"requests": 13, "prompt_tokens": 2548, "output_tokens": 3356, "drafts_offered": 2645, "drafts_accepted": 1578}}
```

声明 `format: json`，`map` 的键写成用点连接的键路径：

```yaml
metrics:
  format: json
  map:
    'live.state!=idle': requests_running
    'live.queued': requests_waiting
    'totals.requests': requests_success_total
    'totals.prompt_tokens': prompt_tokens_total
    'live.prompt_tokens': prompt_tokens_total
    'totals.output_tokens': generation_tokens_total
    'live.generated': generation_tokens_total
    'totals.drafts_offered': spec_draft_tokens_total
    'totals.drafts_accepted': spec_accepted_tokens_total
```

- 路径上的值是数字才采集，布尔值记 1/0；`null`、缺失的路径不出该指标。
- 字符串要写成比较：`路径==词` 或 `路径!=词`，成立记 1、不成立记 0（`live.state!=idle` 就是"正在处理的请求数"）。比较只能映射到 gauge；被比较的值不是字符串时不出该指标。
- 只能映射计数器和 gauge：JSON 文档里没有直方图，TTFT、端到端延迟这类直方图指标由网关统计。
- 不能写 `inherit`，也不会套用引擎内置映射。长写法 `{to, scale}` 照常可用；两个路径映射到同一个指标时相加。
- 注意计数的口径。Strata 的 `totals` 只在请求结束时累加，单映射它，生成吞吐会在长回复期间不动、结束时一次性冒出来。`live` 描述正在处理的那个请求，结束时它已生成的量正好并入 `totals`，所以把 `totals.output_tokens` 和 `live.generated` 映射到同一个计数器相加，吞吐就随生成过程连续增长。
- 这种相加的计数器在请求被取消时会回落（`live` 清空了，`totals` 没加）。平台会把计数器的下降当成引擎重启，把整段读数算成一次增长，所以 agent 对 JSON 端点的计数器做了保持：回落后仍不低于上次读数一半的，保持上次读数，直到计数器重新超过它；跌到一半以下的才当成引擎重启放行。

## 排障

| 现象 | 检查 |
|---|---|
| 面板一直没有引擎数据 | 第 1 步的 curl 是否有输出；`endpoint` 是否与引擎实际路径一致；键是否与原始输出逐字相同（含前缀与冒号） |
| 只有部分指标有数据 | 没数据的那几条在原始输出里是否存在；带标签的族是否需要选择器 |
| 数值差了一个固定倍数（如 100 倍） | 引擎单位与词表不同，补上 `scale` |
| gauge 数值不对 | 同一族有多行，只取了第一行；用选择器挑出要的那一行 |
| 吞吐图平时为 0、请求结束时出尖峰 | 映射的计数只在请求结束时累计（看 `# HELP` 是否写着 finished requests）；改映射包含在途请求的计数，例如 TensorFold 用 `tensorfold_health:completion_tokens_total` 而不是 `tensorfold:generation_tokens_total` |
