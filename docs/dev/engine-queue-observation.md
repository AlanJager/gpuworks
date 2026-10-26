# 逐请求引擎排队耗时的观测边界

监控请求的 `engine_queue_ms` 只能使用同一个推理回复携带的逐请求耗时。
`/metrics` 的排队请求数是瞬时 gauge，服务级直方图是聚合样本，两者都没有逐请求关联，
不能给某个请求反推或回填排队时间。未上报及 JSON `null` 保留为未知；只有显式数字 `0` 才是已测得零。
只有 queue 而没有 prefill 时，时间条保留已知 queue，剩余部分仍为等待，不能伪造零 prefill。

GWKS-93 核对的内置 Strata 镜像版本是 0.1.38。上游固定版本
[`09817da3979abfeb5fbdef7c8f1223d120c1ad39/serve/server.py`](https://github.com/Niko1221/Strata/blob/09817da3979abfeb5fbdef7c8f1223d120c1ad39/serve/server.py)
表明：

- `request_timings`（1624 行）只返回 prompt/decode 等已知阶段，标准回复不包含 queue 耗时。
- 可选 `begin_request`（1232 行）和 `/api/requests`（2147 行）保留内部 `queue_s`，但内部 ID
  在 1237 行独立生成，OpenAI completion ID 在 1751 行另行生成，没有可验证的关联。
- `api_monitor` 默认关闭，开启会保留最近 100 个请求的输入/输出正文（970 行），不宜由平台自动开启。

因此，现有 Strata 回复可以提供 Prefill、Decode，无法可靠补齐某个请求的 20.3 秒排队时间。
真正补齐需要引擎在同一回复中返回 queue 耗时，或提供受支持的稳定请求关联；不得按时间或正文猜测关联。
vLLM 0.25.0 起的 `--enable-per-request-metrics` 已有独立支持，其已有字段由平台读取。
用户侧字段声明及计量口径见[推理指标参考](../reference/inference-metrics.md#逐请求统计)。
