# 测试指南

测试规约的唯一权威是 [`CLAUDE.md`](../../CLAUDE.md)。本文说明如何选择和运行各层测试，
不重复定义质量门槛。

控制台真实浏览器测试遵循 [浏览器 E2E 测试规范](../../test/e2e/browser-e2e-testing-standard.md)：按共享组件与
业务领域盘点，用真实请求、持久化、agent 执行及适用运行时形成证据链。`make test-ui` 与
`npm run test:coverage` 是单元/RTL 测试，不代表浏览器业务覆盖；现有 Python Playwright 入口、
fixture 边界及尚待接入的浏览器门禁见该规范。

> **L3 不得在有在线 `gpuworks-agent` 的机器上运行。** L3 夹具在同一 docker daemon 上启动测试 agent，而 agent 的孤儿清理按 `gpuworks-*` 名前缀删除不在自己期望集合里的容器，会摧毁线上 agent 的实例（2026-09-16 在 gpu3 发生，见 `todo.md` T9）。要在这类机器上跑 L3，先停掉线上 agent，跑完再启动。

## L1–L4

| 层级 | 环境 | 用途 |
|---|---|---|
| L1（simulation） | 单进程、fake/fixture | 纯逻辑、协议、错误和边界；默认离线可重复 |
| L2（multi-process） | 本机多个真实进程 | server/agent 重连、流、进程退出和恢复 |
| L3（container） | 本地 Docker/udocker/nerdctl | 容器网络、挂载、runtime CLI 与 fake engine |
| L4（cloud GPU） | 显式真机或云资源 | GPU/驱动/厂商 runtime、真实下载和推理验收 |

```bash
make test
make test-integration
make test-coverage-report
```

提交前还要对相关包运行 `go test -race`、对带显式 build tag 的测试集做 compile-only（只编译、不执行）、运行
`make vet` 和 `git diff --check`。缺少外部能力时普通测试才可 `t.Skip`；
`GPUWORKS_TEST_STRICT=1` 下关键依赖缺失应失败。

## 负载用例（默认 suite 之外）

这些用例测量实际经过的时间（墙钟时间）和资源消耗，默认测试集（suite）不跑，`GPUWORKS_TEST_PERF=1` 显式开启。
下文的 fleet 是批量运行 agent 的测试集群；churn 指反复执行部署、拉镜像、进入 running、删除的循环。
CI 引用的两个多进程用例有 Make target；其余直接用 `go test` 跑，完整命令（含 `-tags`、`-timeout`、缩放变量）写在各测试函数的注释里：

| 入口 | 用例 | 测什么 |
|---|---|---|
| `go test`（见 `skipUnlessPerf` 注释） | `internal/integrationtest/tunnel` 的 `TestLanes_*` | 双通道吞吐与 ctrl 延迟 |
| `go test`（见测试注释） | `TestFleet1000NodesRedeployChurn` | 大规模节点接入 + 反复部署时控制面的写入压力（单进程） |
| `make test-fleet-perf-processes` | `TestFleet1000NodesRedeployChurnAcrossProcesses` | 同一负载，但 server 是独立进程、fleet 是另一个进程 |
| `go test`（见测试注释） | `TestFleetLogCenterScale` | 日志中心在 fleet 负载下的读目标（单进程） |
| `make test-fleet-log-scale-processes` | `TestFleetLogCenterScaleAcrossProcesses` | 同一日志中心负载，server 与 fleet 分进程 |
| `go test`（见测试注释） | `TestFleetGatewayLoadAcrossProcesses` | `/v1` 数据面：server 进程 + 多卡 fleet（流式 fake 引擎）+ 流式客户端群，报告 req/s、TTFT/E2E、server 核数与内存、副本分布；`GPUWORKS_GATEWAY_*` 调形态，基线见 `artifacts/archive/2026-09-24-gateway-load-acceptance/gateway-load-gpu2-2026-09-24/` |

fleet 用例默认 1000 个 agent，其中一半循环执行 部署→拉镜像→running→删除。数据库写入磁盘，
以测量真实写入代价；`:memory:` 不执行 fsync（把数据同步到磁盘），测不出这部分开销。用例断言：

- 在线节点的心跳不会超过 60 s 不更新（心跳排在进度写入后面，就是本次治理的目标故障）；
- churn 结束时没有节点仍处于 offline；
- 每次部署都能进入 running；
- 每次部署的事件行 ≤ 20。

另外记录（只报数不断言）：churn 期间掉线又重连的节点数、`GET /deployments` 与事件接口的
p50/p99（第 50/99 百分位耗时）、写入静止时的 `GET /deployments` 单次耗时、数据库增长、
RSS（进程驻留内存）与 CPU。

### 两种形态，同一套驱动

从「节点全部上线」到「churn 结束」之间的全部代码都在 `internal/integrationtest/fleet`
（REST 客户端、节点存活观测、churn 循环、报表与判定），两种形态共用，只有进程拓扑不同：

- **单进程形态**（`internal/integrationtest/fleet`，默认 suite 之外）：server 与 1000 个 agent 同处
  一个测试进程。最省事，但它的 RSS/CPU/调度器是两者之和，**回答不了「控制面自己要多少」**。
  模型与 model file 由 store 直接写入（属于测量窗口之外的 setup）。
- **多进程形态**（`test/l2`，`-tags=multiproc`）：真实 `gpuworks` 二进制作为独立进程，另一个
  `test/l2/cmd/fleet-host` 进程托管 N 个 agent（真实 agentapp + 同一套 fake prober/driver）。
  节点只能走 REST（`POST /api/v1/nodes`）拿 token，模型走产品自己的「注册节点上已有目录」
  路径（`POST /api/v1/models/local` 与 `/models/{id}/register-path`），不下载一个字节。
  两个进程的 RSS 与 CPU 分别从 `/proc` 读取。

**多进程形态是权威的规模 case**：只有它能把 server 自身的开销与 1000 个 agent 的开销分开，
数字也更稳定；单进程形态保留作小规模快速试跑。两者都不进默认 CI。GitLab 里由
`l2-fleet-redeploy-churn` job 跑多进程形态，只在 `FLEET_REDEPLOY_CHURN=1` 的 schedule / web / api
流水线里出现（每晚定时任务带这个变量）；job 以 `GPUWORKS_TEST_STRICT=1` 运行，文件描述符上限不够
会失败而不是跳过，两个进程的日志作为 artifact 保留 14 天。掉线重连数与接口 p99 目前只报数不断言，
看 nightly 的趋势，积累足够运行后再定阈值。

多进程形态可用预编译二进制运行，**没有 Go 工具链的机器只能这样跑**：

```bash
GPUWORKS_FLEET_SERVER_BIN=/path/gpuworks GPUWORKS_FLEET_HOST_BIN=/path/fleet-host \
GPUWORKS_FLEET_WORK_DIR=/path/work make test-fleet-perf-processes
```

`GPUWORKS_FLEET_WORK_DIR` 把本次运行的数据库、agent 状态与日志放到指定目录（失败后可以现场
排查）；`GPUWORKS_FLEET_PROCESS_LOGS=1` 另存两个进程的 stdout/stderr。在已经跑着线上服务的
机器上，用私有 network namespace（独立的网络命名空间）隔离端口（引擎端口固定从 30000 开始分配）。
下面是命令框架，`...` 要替换为本节所选的实际测试命令：

```bash
unshare -n bash -c 'ip link set lo up; ulimit -n 65536; ...'
```

### 掉线重连

掉线重连只报数不断言。以下保留该指标的故障诊断与修复背景，便于理解当前判定方式。
之前把掉线归因为「一个进程同时跑 server 和 1000 个 agent，CPU 不够」，
实测否定了这个解释：换到 20 核空闲机器上抖动依旧。传输层存在一个竞态，`internal/wsconn`
修复了其中一条触发路径，另一条路径由后文的 keepalive 间隔调整解决。原来的触发过程是：

1. go-yamux 的发送循环（send loop）只在连接写超时周期过半时，才更新写截止时间。
2. coder/websocket 的 `net.Conn` 用一个「已过期」粘滞标志实现写超时。`SetWriteDeadline`
   会清标志并重排定时器，却无法撤回已经放行的回调；旧回调晚到，就会在续期之后重新把标志置为已过期。
3. 下一帧尚未写入 socket，就直接以 `failed to write: context deadline exceeded` 失败。
   yamux 把发送循环出错视为整个会话的致命错误，server 随即执行 `deregister`，节点变为 offline。

两端的 `laneKeepAliveInterval` 恰好是
`laneConnWriteTimeout` 的一半，空闲会话的 ping/pong 因此反复落在写超时那一瞬间，命中概率很高。

`internal/wsconn` 的回归用例（`TestLiveWebSocketSurvivesAWriteAtTheDeadlineInstant`）用真实
WebSocket 复现：未包装时 16/16 连接失败且全部耗时 <5 ms（根本没写 socket），包装后为 0。

但 `wsconn` 只能兜住「回调到来时没有写在进行」的那一支。同一个回调若撞上正在进行的写，会直接
cancel 适配器的写 context 且永不更换，coder/websocket 见写 context 被 cancel 就整条关闭连接，
之后的写也全部失败——这一支无法在包装层补救。线上（mirror1）每个节点每天仍掉线 40–100 次、
断开时刻严格对齐 15 s 周期、抓包显示关闭方在发出自己的 keepalive ping/pong 后不到 1 ms 就 FIN，
就是这条路径；本地回环同样复现（17 分钟 7 次）。

根治是把两端 `laneKeepAliveInterval` 从 15 s
降到 10 s，让 keepalive 写入总是早于写超时过半点至少 5 s 续期，永远不会踩到到期瞬间；
`TestLaneYamuxConfig_MatchesPlan`（server）与 `TestLaneYamuxConfig_KeepsKeepAliveClearOfTheWriteDeadline`
（agent）守住这个余量。改后本地回环 25 分钟零掉线，100 个 web terminal 并发满速刷屏亦零掉线。

剩余的抖动是主机真的被打满时丢了会话又立刻重连，因此仍然只报数。**节点掉线后没回来**是另一
回事，会断言。诊断时用 `GPUWORKS_TESTENV_LOG=<path>` 打开 server/agent 日志，用
`GPUWORKS_FLEET_DUMP_DIR=<dir>` 在心跳变陈旧或节点掉线的当下抓 goroutine/mutex/block dump
（`GPUWORKS_FLEET_DUMP_TRIGGER_MS` 调阈值）。

### 单进程形态的收尾

单进程形态必须在用例结束时停止本用例的 agent，并关闭客户端连接，否则收尾会因为与被测行为无关的原因失败：

- `env.StopNodes(t)` 取消全部 agent 并等待 hub 的会话表清空。`AddNode` 的 cleanup 只取消
  context、不等待，server 关停于是和上百个正在拆解的会话抢时间，超出 testenv 的 5 s 关停预算。
- `client.CloseIdleConnections()` 关掉 churn 留下的 keep-alive 连接。用例自己开着几百条空闲
  连接时，`http.Server.Shutdown` 要等它们，实测这就是 1000 节点落盘用例约一半概率出现
  `server shutdown timeout` 的原因。

多进程形态没有这个问题：server 是独立进程，收尾就是发信号。

### 接入期间的列表响应

多进程 fleet 用例先接入半数节点并观测稳定状态，再接入剩余节点；默认即 500→1000。
每个接入阶段和稳定阶段分别并发读取节点全量列表、节点分页列表、部署列表和实例列表，输出样本数、
p95、最大耗时与失败数。每个接口的接入阶段至少 1 个成功样本，稳定阶段有界等待至少 3 个成功样本；短接入阶段只等待已发出的
请求完成，不把接入后的新请求算成接入样本。请求必须返回 HTTP 200、完整读取响应体，
且单次请求不超过 10 秒；超时、错误响应、读取失败和样本不足都不能判通过。
churn 期间的部署/事件列表同样检查错误、样本和 10 秒上限。

部署列表已使用批量关联查询，不能再把历史 N+1 描述当作当前性能根因。节点接入还会触发镜像恢复和
存储范围解析：这些读取必须限定当前节点及其相关传输/标签目标，避免每个新节点遍历整份镜像目录或
所有节点的存储覆盖。`GPUWORKS_FLEET_READS=0` 仅关闭 churn 的并发读，用于隔离 churn 代价；
多进程接入阶段的容量检查始终执行。

### 缩放与前置条件

按机器能力缩放，三个环境变量两种形态通用：

```bash
GPUWORKS_FLEET_NODES=200 GPUWORKS_FLEET_CHURN_NODES=100 GPUWORKS_FLEET_ROUNDS=1 \
GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 60m ./internal/integrationtest/fleet/ -run '^TestFleet1000NodesRedeployChurn$'
```

文件描述符（fd）需求约为 `6 × 节点数`；用例会尝试把 `RLIMIT_NOFILE` 软限额抬到硬限额，抬不上去时
`GPUWORKS_TEST_STRICT=1` 下失败、否则明确 Skip（不静默跑在不足的限额下）。非 Linux 主机
直接 Skip：那里的 fd 限额与 RSS 读法不同，数字无法与 CI 对比。

## 公网入口（expose）

`internal/server/expose` 的测试全部离线：假 cloudflared 通过 `PATH` 提供（`--version` 与注册
日志都是真实输出形状）。用于判断地址是否 ready 的网络健康检查由可注入的 `HealthChecker` 承担；
测试替换这一检查，验证 ready 判定，不依赖真实网络。进程相关用例跑真子进程：`connector-host`
helper 由测试二进制充当，覆盖 Stop 回收、SIGKILL 父进程后无孤儿（含启动竞态，多轮）、退出码
传递，以及「server 拿不到 hosting 锁时不托管」。上游通信数据格式（wire 形状，包括 cloudflared `/config`、
deb 索引、rpm repomirror 元数据）全部用 fixture 覆盖，fixture 用的是实测抓下来的真实报文。

**不新增依赖联网的永久测试**（用户确认）：下载、解包、安装的回归全部走合成包与本地 HTTP
fixture，不访问真实包源、不调用包管理器、不新增 Make target 或 CI job，也没有靠环境变量
开关的"特殊用例"——那仍然是一份会腐烂的联网测试。开发期的人工联网验证按需执行，记录版本、
环境与结果，但不固化成用例：

- **包源漂移**（fixture 是否还像上游）：`curl -s https://pkg.cloudflare.com/cloudflared/dists/any/main/binary-amd64/Packages | grep -E '^(Version|Filename|SHA256):'`
  与 `curl -sI https://pkg.cloudflare.com/cloudflared/rpm/x86_64/cloudflared-linux-x86_64.rpm`
  看 deb 索引字段、rpm 是否仍无版本号路径。解析器与 SHA256 校验的权威回归是 fixture 用例；
  这一步只回答"fixture 要不要重抓"。
- **端到端获取**：在一台 PATH 上没有 cloudflared 的主机上跑 `gpuworks expose up`，确认
  下载、解包、`--version` 自校验与随后的注册都正常。
- **quick / named / agent**：注册与新地址可用、公网 `/healthz`、named 的 token-only 与路由
  发现、SSE 首包与取消、真实 agent 接入与断网恢复——均为开发期人工验证，不固化成用例。

**named 的真机验收在 2026-09-22 只完成了一半**（用户随后决定暂不开放该提供方，见下）：实机确认了
连接器自动获取（`pkg.cloudflare.com` 下载 + 不可变版本路径启动）、只给 token 启动、注册成功
（`Registered tunnel connection`，边缘/协议归观测）、远端未发布路由时的 `awaiting_route` 与指引、
以及该 tunnel 只有默认 catch-all（`version:-1`、`http_status:503`）时不被误判为可用地址。

**未证**：
首次公网可达（`ready` 提升）、SSE 首包/取消、agent 接入、重启后域名不变——这些都需要用户先在自己
的 Cloudflare 账号里发布一条路由，而 named 现在不对用户开放（`--provider cloudflare-named` 会被
明确拒绝），所以这几条在放开之前无法验收，也不得写成已通过。

## 架构门禁

`make check-architecture`（已并入 `make test-unit`，CI 与 `check_time_policy.sh` 同 job）
执行两类检查：

- **架构约束**：`scripts/arch-constraints.txt` 每行是 `键<TAB>限额<TAB>方向`。`max` 表示实测值
  必须 ≤ 限额，`min` 表示必须 ≥ 限额。脚本逐项打印 `键 实测/限额 (方向)`，超限即失败。
- **依赖方向**：叶子包不依赖 store、`api` 与 `controller` 互不依赖、`pkg` 不依赖 `internal`、
  agent 不依赖 server、`proto` 无本仓库依赖。这些是结构规则，不是预算，违反即失败。

棘轮规则：`max` 键的限额**只降不升**，`min` 键的限额**只升不降**。实测值优于限额时脚本打印
WARNING（不失败），提示在同一个 MR 里把限额收紧到新实测值。任何抬高 `max` 限额或降低 `min`
限额的改动都必须在 MR 描述里解释并获得显式批准。

每条预算的意图、实测命令与排除清单见 [`scripts/arch-constraints.md`](../../scripts/arch-constraints.md)；
误报应当修模式，不应当调预算。

过程/里程碑标签（`M1`、`S0`、`G2`、`P3` 这类阶段编号）**禁止出现在生产代码**里——包括标识符、
注释和文件名，由 `process_name_refs` 预算守护。计划一旦失效，这类标签就变成读者无法核对的
误导性注释；应改写为对行为的描述。测试文件不受此限制，可以自由引用具体阶段。

真机用例统一为 `l4-<provider或资源池>-<行为>-<拓扑或外部依赖>`，Go case、Make target
与 CI job 一一对应。可验证行为使用 `node-onboarding`、`public-download`、
`deploy-chat`、`provisioning-matrix`、`one-vm-two-logical-nodes` 或
`two-vm-docker-distributed`；`nightly` 和 `manual` 只描述触发策略。

仓库当前提供 RunPod、AutoDL、Vast、CompShare 和昇腾资源池的显式 `make test-l4-*`
target。先以 `make help`/Makefile 确认目标，再只运行所需的测试路径（lane）。

## 成本和缓存

- provider 常规用例只验证租机、SSH、agent 上线与 GPU inventory。RunPod、CompShare、AutoDL、
  Vast、ZStack 的 API 字段、区域约束和清理方式由各 provider 实现决定，用例按各自实现写，
  不假设统一行为。
- 公网下载只由 `test-l4-runpod-huggingface-public-download` 和
  `test-l4-compshare-modelscope-public-download` 等显式低频 `public-download` case 验证，
  避免每次租机都重复拉镜像和模型。
- CompShare 可通过 `COMPSHARE_GOLDEN_IMAGE_ID` 复用预热 VM 模板，通过
  `COMPSHARE_DOCKER_IMAGE_TAR` 指定 VM 内可见的镜像归档，通过
  `COMPSHARE_PRELOADED_MODEL_PATH` 注册每台节点已存在的完整模型目录。
- `prepare-compshare-golden-image` 创建并验证新模板，但不会删除旧模板或修改 CI 变量。
  模板里同时存放同一版 NVIDIA 驱动（当前 570.153.02）的开源与闭源两套内核模块，开机时
  `gpuworks-nvidia-kmod.service` 先加载开源版（RTX 5090 等 Blackwell 卡只能用它），没有初始化出 GPU
  时换成闭源版（V100S、P40 这类没有 GSP 的卡只能用它），选中的是哪套写在 `/run/gpuworks-nvidia-kmod`。

  工具做完模板后按 `-validate-gpus` 逐个卡型开机验证，也可用 `-validate-image <id>` 只验证已有模板；
  没货或被 CompShare 拒绝的卡型只报告不判失败。工具也会在镜像缺 nvidia-container-toolkit 时装上与华北模板相同的版本并配置 Docker 运行时
  （上海那条模板线原本没有，容器里拿不到 GPU）。

  自定义模板按地域隔离，本账号的接口不支持跨地域复制，
  各地域要分别以该地域已有的模板为源制作（`-region`/`-zone`/`-source-image`，上海只有 2080Ti×4
  时用 `-gpu-model 2080Ti -gpu-count 4`），结果写入 `COMPSHARE_GOLDEN_IMAGE_ID` 与
  `COMPSHARE_ZONE_IMAGE_CN_SH2`。

  CompShare 不允许自定义模板用于 A800、H20
  （`RetCode 226603`），与驱动无关；模板名超过 42 个字符会在建模板时被拒（`RetCode 230`）。

失败时优先冻结现场并查看进程/容器日志：先保留失败实例、记下本次运行的 run-id 和 SSH/agent 日志，
完成排查后再做 scoped cleanup，即限定本次运行范围的清理。云用例可按实现支持设置
`GPUWORKS_CLOUD_KEEP_ON_FAILURE=1`，之后只用本次 scoped run-id 调用
`test-l4-cleanup-leaked` 或相应清理入口；禁止无范围清理整个账号。云端 L4 结束后要按 run-id
确认没有实例、转发或临时文件泄漏。产品侧的租机操作见[租用云 GPU](../how-to/rent-cloud-gpus.md)。

## 证据

测试方法写在本文；一次性运行日志、截图、镜像 digest 和环境事实写到
`artifacts/work/<YYYY-MM-DD>-<主题>/<主题>-<YYYY-MM-DD>.md`（收口后随主题搬进
`artifacts/archive/`），现行文档只链接证据。

