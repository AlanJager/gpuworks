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
  在 1237 行独立生成，OpenAI completion ID 在 1751 行另行生成。非流式详情可从完整回复正文找到
  completion ID，但摘要和流式监控没有不依赖正文的稳定关联。
- `api_monitor` 默认关闭，开启会保留最近 100 个请求的输入/输出正文（970 行），不宜由平台自动开启。

因此，未经适配的 Strata 回复可以提供 Prefill、Decode，无法可靠补齐某个请求的 20.3 秒排队时间。
需要引擎在同一回复中返回 queue 耗时，或提供受支持的稳定请求关联；不得按时间或正文猜测关联。
vLLM 0.25.0 起的 `--enable-per-request-metrics` 已有独立支持，其已有字段由平台读取。
用户侧字段声明及计量口径见[推理指标参考](../reference/inference-metrics.md#逐请求统计)。

## 内置 Strata 的适配

Strata 目录通过已有运行时资产机制携带轻量补丁和启动包装器，基础镜像保持不变。
包装器只接受固定 0.1.38 源码或已应用的补丁哈希，原子替换容器可写层中的 HTTP handler，
然后原样转发参数并进入原镜像启动流程。未知源码或不可写的旧源码会明确启动失败；
资产本身以内容哈希校验并只读挂载。构建和校验入口见 `build/strata-queue/`。

补丁以单调时钟测量首次加载的 FIFO 等待和首次生成的 FIFO 准入等待，在同一流式或非流式回复的
`timings.queue_ms` 中上报毫秒数。模型加载、prefill、decode 和 MCP 工具执行不计入该值；
首次生成准入后冻结计时，后续 MCP 轮次的排队不能被画到首 token 之前。失败且未产生成功终态耗时的请求仍为未知。
平台使用目录中的 `timings.queue_ms → queue_seconds` 声明读取该值，不开启 `api_monitor`，也不保存正文用于关联。

该适配只随更新后的目录配置启动。已经运行的旧实例不会被在线修改；更新部署到该配置并重建实例后才会输出新字段。
这提供新的逐请求观测能力，不能追溯恢复原故障请求已经丢失的 20.3 秒证据，也不修复原生引擎卡死。
