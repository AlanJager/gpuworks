# ADR 0039: 推理指标的来源分层，观察配置由 server 唯一解析、运行中原地更新

日期：2026-10-10 · 状态：已接受

## 背景

callable 部署（TensorFold、包了一层 SGLang 的适配器、以 callable 跑的 vLLM 改版）看不到任何推理指标。agent 每 10 秒抓实例的 `/metrics`，
但映射写死在 vLLM / SGLang 的引擎 Profile 里；callable 只按 `gpuworks_callable:` 前缀兜底，没有引擎导出这个前缀，抓到的内容全部被丢弃。
模板没有字段能声明指标端点或映射。平台指标名（`gpuworks_infer_*`）是散落在引擎映射、监控、网关和前端里的字符串，没有一处定义「平台有哪些推理指标、
是什么类型和单位」。

部署快照冻结了启动配置，而指标映射只决定平台怎么观察实例、不决定实例怎么跑：映射写错或引擎升级改名后，要求用户重新部署才能看到数据，代价与收益不成比例。

## 决策

1. **平台推理指标只在叶子包 `internal/inferencemetrics` 定义一次**：短名、类型（counter / gauge / histogram）、单位，映射规则的类型与校验，
   vLLM / SGLang 的内置映射，以及派生指标（KV 使用率由池容量和空闲量算出）。存储名是 `gpuworks_infer_<短名>`，已有名字全部保留。
   agent 与 server 共用它，它只依赖 `internal/engines`。引擎 Profile 的 `MetricMap` 改为从这里派生的视图，不再是第二份拷贝。
2. **模板在 `api` 契约里声明 `metrics`**：`endpoint`、`inherit`（vllm / sglang）、`map`（引擎族 → 短名，可带标签选择器，长写法可换算）。
   生效映射 = 继承映射逐条叠加模板 `map`，**模板优先**。目标用短名，导入时按词表校验并给出稳定错误码。不新增表或列：`api` 本来就是 spec 的整体读写 JSON
   （`inferenceapi.Contract`），`metrics` 是它的一个键。
3. **callable 不声明时按前缀自动识别**：照旧抓 `/metrics`，兜底由找 `gpuworks_callable:` 改为「抓到 `vllm:` 族就套 vLLM 映射，否则有 `sglang:` 族就套 SGLang 映射」，都没有则不出引擎指标。
4. **观察配置由 server 唯一解析**：`deploy.ResolveObservation(engine, api.metrics)` 产出 `proto.InstanceObservation`（端点、规则，或自动识别的候选规则集，
   目标已展开为存储名），写进实例期望状态。agent 只执行：抓取、挑规则、映射、换算、派生、上报，不做任何决策。
   同时照旧填写 `MetricPath` / `MetricPrefix` / `MetricMap` 旧字段，老 agent 按旧逻辑采集；带换算的规则不进旧字段，以免老 agent 存错单位。
5. **观察配置取当前模板，运行中原地更新**：启动与刷新都读部署所用档位（模板行 + spec 来源 + spec 名）当前的 `api.metrics`；档位已删除时退回部署快照里的 `api` 契约。
   目录写入（导入、编辑、内置目录 reconcile）在事务提交后发布 `inference_template` 事件（带模板行 ID，或「全部」）；controller 合并事件，
   重新解析引用这些模板的部署的实例，只把观察配置变化了的实例在一个事务里改写，再按节点推送。server 启动时做一次全量刷新。
6. **原地更新走独立消息，不走期望状态替换**：`StateUpdate.Observations []ObservationUpdate` 只携带实例 UUID 和新的观察配置。agent 在已持有的实例上替换观察字段，
   不重新接管容器、不重新探活、不上报状态；不认识的 UUID 忽略。老 agent 不认识该字段，原样忽略，在下次 `state.sync` 时从存储的期望状态拿到新配置。

## 取舍

- 放弃「把新期望状态整份重推、让 agent 比对后跳过」：`ApplyUpdate` 会重新接管容器、重新探活，可能把运行中的实例短暂报成 starting 甚至回收；
  而重放时补齐的 `ImagePullRefs` 等字段让「只有观察配置变了」的判断在 agent 侧不可靠。独立消息让「只换观察配置」在协议上就成立。
- 观察配置脱离部署快照：部署看到的映射可能和创建时不同。这正是目的——映射只影响观察，不影响运行；`api.metrics` 也因此不参与「是否偏离模板」的判断。
- 老 agent 拿不到换算、派生与自动识别；它们随 agent 升级生效。换算规则从旧字段里去掉，宁缺不错。
- 内置映射从引擎 Profile 移到叶子包：引擎事实与词表放在一起，`inherit` 和自动识别引用同一份规则；Profile 保留派生视图供现有调用方使用。
- 刷新只在目录变化时触发，按模板行 ID 走索引查部署和实例，解析结果相同的实例不写库、不推送；启动时的全量刷新是一次性开销。

## 后果

- TensorFold 等改了前缀的引擎靠模板声明接入面板；沿用 vLLM / SGLang 命名的引擎以 callable 部署不写声明也有数据。
- 模板作者改映射后，运行中的部署在下一轮抓取（≤ 10 秒）生效，无需重启或重新部署。
- 网关测量每个转发回复的 TTFT、耗时与引擎报告的 usage token 数（`gpuworks_proxy_*`，名字同样登记在本词表）。监控按信号取来源：TTFT、端到端、输入/输出 token、运行请求在引擎上报时用引擎值，否则用网关值，唯一决定处是 `monitor` 的信号来源目录（`window_sources.go`）；界面不标注来源，口径差别写在监控文档。
- 文档：[推理指标词表](../reference/inference-metrics.md)、[`api.metrics` 语法](../reference/inference-catalog-spec.md#apimetrics引擎指标映射)、
  [为推理模板声明引擎指标](../how-to/declare-engine-metrics.md)。
