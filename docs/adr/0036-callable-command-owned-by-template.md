# ADR 0036: callable 的命令行归模板，平台事实以模板变量给出

日期：2026-10-08 · 状态：已接受

## 背景

callable 引擎承载目录自带的 Serve Adapter。此前它在模板参数前固定追加 `--model <路径> --host 0.0.0.0 --port <端口>`，
adapter 必须认识这三个 flag。

多机部署需要更多平台事实：节点数、本 rank 序号、master 地址与端口、是否 headless。这些值只在 controller
为某次启动分好 rank 之后才存在，写模板时无从得知；而模板可用的变量只有 `GPU_COUNT`、`MODEL_PATH`、`TP_SIZE`
与 resolver 输出，且只在 spec 带 placement 时才展开。曾有分支沿用 vLLM/SGLang 的做法，让 callable 再追加
`--tensor-parallel-size`、`--nnodes`、`--node-rank` 等 flag。这样做会打断所有不认识这些 flag 的 adapter：内置目录的
`gpuworks_serve.py` 用 `argparse.parse_args()` 严格解析，单机部署也因多出的 `--tensor-parallel-size 1` 直接退出。
另一条路是 adapter 自己猜 rank（比如按本机 IP 判断），这把测试环境写死进了模板。

vLLM、SGLang 的词表是固定的，平台知道该注入什么；callable 的命令行由模板作者定义，平台按固定形状追加任何东西，
都是替作者决定了 adapter 的 CLI。

## 决策

1. **callable 不追加任何参数**。它的 argv 就是模板参数加用户参数，可执行文件通常由模板 `entrypoint` 给出。
2. **平台事实统一作为模板变量提供**，所有引擎都能用：`MODEL_PATH`、`API_HOST`、`API_PORT`、`NNODES`、`NODE_RANK`、
   `HEADLESS`、`GPU_COUNT`、`TP_SIZE`，多机时另有 `MASTER_ADDR`、`MASTER_PORT`。词表只在 `internal/server/placement/expand.go`
   定义，取值只在 `deploy.ResolveLaunch`（`launch_variables.go`）计算，模板自己决定用哪些、摆成什么形状。
3. **展开不再以 placement 为条件**，每次启动都展开。
4. **平台事实不能由 resolver 返回**：resolver 运行时还没有节点、端口和 rank，它给的值只能是猜测。
5. **缺变量即失败**，不做静默兜底：
   - 部署时，argv、env 中未解析的名字报错。
   - 导入时，没有 resolver 的 spec 引用词表外的名字直接拒绝。
   - 单机部署没有 `MASTER_*`。
   - `pre_start_script` 由 shell 执行，词表外的名字留给 shell。
6. **callable spec 必须引用 `${API_PORT}`**。网关与健康检查访问的是平台保留的端口，只有模板能把它告诉 adapter。
   导入时检查，部署时对引擎模式再检查一次。部署自己的命令覆盖接管了整条命令，不在检查之列。

## 后果

- 内置目录的 7 个 callable 档位改为在 args 开头显式写 `--model ${MODEL_PATH} --host ${API_HOST} --port ${API_PORT}`，
  生成的 argv 与改动前逐字相同。
- 已有的 callable 部署由迁移 130（`callable_template_args`）在其目录选择快照的模板参数前补上同样三对变量，
  重启后的 argv 与升级前逐字相同。
- 已导入数据库、且没有引用 `${API_PORT}` 的用户 callable 档位不迁移：基于它新建部署时会被明确拒绝，需要重新
  导入改好的包。不保留旧的注入行为作兼容模式。
- 托管可写目录（如 KV 磁盘层）目前只能通过 vLLM 的参数字典申请，callable 申请不到，所以暂不提供对应变量；
  将来给 callable 增加通用托管目录时，按「路径 + 容量上限」成对加入词表。
