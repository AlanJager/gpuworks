# ADR 0079: 引擎指标可以在 API 端口之外的第二个端口上：`api.metrics.port: separate`

日期：2026-11-20 · 状态：已接受

## 背景

agent 每 10 秒抓 `http://127.0.0.1:<API 端口><path>`（ADR 0039），模板的 `api.metrics` 只能改路径与映射。有的 `callable` 引擎独占 API 端口，那里只有 `/health`、`/v1/models`、`/v1/chat/completions`，没有 `/metrics`（例如用 Rust 前端占住 API 端口的引擎）。镜像里的 launcher 能从引擎的管理 socket 和日志读出指标，在第二个本机端口上导出，不代理请求；平台缺的是：为它保留这个端口、告诉 launcher、让 agent 抓它。

## 决策

1. **模板只声明形态，不写端口号**：`api.metrics.port` 取 `api`（缺省，存为无值）或 `separate`。端口与 API 端口、PD side channel 一样由平台分配（`pickPorts`，与 API 端口同一次分配、通常紧邻其后），避免模板作者与别的实例、宿主机监听冲突。
2. **一个启动变量**：声明后平台给出 `${GPUWORKS_METRICS_PORT}`（旧写法 `${METRICS_PORT}`），沿用启动变量的一种写法规则（`placement/expand.go`）；它是启动独有的事实，resolver 不能返回。headless rank 与 API 端口一样没有，值为 `0`，同一份命令可在所有 rank 上展开。
3. **变量与声明成对**：导入、目录包上传与编辑器保存时，引用变量却没有声明报 `CATALOG_METRICS_PORT_UNDECLARED`，声明却没有引用报 `CATALOG_METRICS_PORT_UNREFERENCED`，`port` 写成别的值报 `CATALOG_METRICS_PORT_INVALID`。没有声明时引用变量在启动时也同其他未解析变量一样失败。
4. **端口是启动的事实，随期望状态存下**：保留的端口写入 `InstanceDesiredState.metrics_port`（缺省 0 即 API 端口，旧数据无需迁移）和实例的 `ports` 列表（保证后续分配避开它，API 端口之后为第二项；PD leader 为 API 端口、指标端口、side channel 块、集合通信端口）。agent 重启或容器重启沿用它。是否保留取部署创建时的模板快照（`CatalogSelection.APIContract`）：该快照在部署使用模型自己的操作时会被换成模型的契约，只有 `metrics.port` 被保留下来，`endpoint`、`format`、`map` 仍读档位的当前声明（ADR 0039）。
5. **只抓有端口的 rank**：与今天一样只抓对外服务的实例（单机、多机 leader、PD 的每个 leader）。agent 抓 `http://127.0.0.1:<metrics_port><endpoint>`；`metrics_port` 为 0 时仍抓 API 端口。
6. **预览与启动同源**：部署评估用占位端口（`planning.PreviewMetricsPort`），创建时换成保留的端口；从部署导出模板时把声明带回去，否则导出的参数引用了一个没有声明的变量。

## 取舍

- **不是 `port: <数字>`**：写死数字在同节点多实例、多副本时必然冲突，平台已经有端口分配器，模板没有理由绕开。
- **不是代理**：平台不转发请求，也不把指标端口暴露给网关；它只被本机 agent 抓取。
- **不放进 Observation**：Observation 是服务端可以在实例运行中原地更新的部分，端口却是启动时保留的事实，改模板不能让一个已经在跑的实例凭空多出一个端口。放进期望状态与 `ports`，与 API 端口同一个生命周期。
- **不新增列**：端口写进已有的期望状态 JSON（具名类型 `proto.InstanceDesiredState`）与 `ports` 列；缺省值就是旧行为，读旧数据不需要迁移。
- **旧 agent 不认识该字段**：它仍抓 API 端口，只是没有引擎指标，不影响部署，所以没有为它加 agent 能力。

## 后果

- 语法与行为：[推理目录 model.yaml 语法参考的 api.metrics](../reference/inference-catalog-spec.md#apimetrics引擎指标映射)；写法：[为推理模板声明引擎指标](../how-to/declare-engine-metrics.md#指标在另一个端口上)。
- 协议：`InstanceDesiredState.metrics_port`。
- 新增错误码：`CATALOG_METRICS_PORT_INVALID`、`CATALOG_METRICS_PORT_UNDECLARED`、`CATALOG_METRICS_PORT_UNREFERENCED`（en/zh 文案同改）。
