# ADR 0003: 压测引擎内置于二进制，不依赖外部工具与容器

替代原 EvalScope 容器方案：压测负载生成器作为纯 Go 库（`pkg/bench`）内置进 server 与 agent 二进制，发压全程不拉镜像、不起容器、不依赖 Python。原方案的失败教训：结果解析靠容器内嵌 Python 硬编码 EvalScope 字段名，上游字段漂移会静默产出全 0；拉镜像慢且离线环境不可用；all-or-nothing 无进度无部分结果。代价是自己实现负载生成、指标累计与定长输入合成（含纯 Go tokenizer 精确模式 / 语料估算模式两档降级），换来的是结果 schema 完全自持、售后 SSH 上节点即可用 `gpuworks-agent bench` 单独压测。

旧的容器式 jobs 管道（`store.Job`、agent jobEngine、evalscope 插件）随之整体删除，需要容器型任务时从 git 历史找回。
