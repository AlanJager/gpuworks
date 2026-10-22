# E2E 进度交接

## 第二批 E+F（2026-10-22 交付完成）

- Goal：模型文件下载/失败重试/实际字节与 hash/指定副本删除，及部署向导公开来源/存储/自动放置；同步登记组件分母与缺口。
- 独立 checkout：`/home/test/gpuworks-e2e-second-batch-20261021`；分支 `test/e2e-model-acquisition-20261021`；启动最新 master `2b77e73aa33a98f42d7d246cf3e73418299bf2ef`。
- 最终 HEAD `552d58c56`（含 wei/master `5efef9391`，两次 master 漂移合并 6c8753be9 / 552d58c56 均独立复审 APPROVE、合并内容与 master 增量逐字节一致）。MR !592 非 Draft、不自动合并。
- 最终验收在独立 clone `/home/test/gpuworks-e2e-second-final-20261021`（非 worktree；wei/master 冻结 tag `frozen-wei-master-20261022`）：`make test-e2e-second-batch` 14/14 PASS、零重试，每案例 current ordinary PASS + case_completed + artifact_scan PASS + 清理观察成功；runId 与二进制版本见 `secrets/e2e/second-batch-final/batch-report.json`（source 552d58c56 / target 5efef9391，批次内逐案例复查源码/target/二进制零漂移）。
- 变异 7/7 击杀于预定断言（control/mutant/control-after 链全绿、零残留；F7 control-after 两次高负载 5s UI 超时抖动已留现场，负载回落后 PASS——weight-source 弹窗 5s 默认超时对负载脆弱，建议属主关注）。证据：`secrets/e2e/mutations/<name>/`（metadata.json + mutation.patch + 三跑报告 + F7 抖动现场）。
- 复审台账（全部未参与编写的 subagent、零新增零遗留）：批次入口 R1、model-download fixture R2、acquisition fixture/browser R2、model-files 线程 R4、F1 矩阵 R4、console/HTTP 归因 R4、覆盖率线程 R1→R6 终审（失败关闭白名单化 + 空循环折叠 + 文档收敛，770 行投影 diff 为空）全部 APPROVE；评审文件 /tmp/gpuworks-e2e-second-*.md。
- 冻结 HEAD 门槛：完整 `make test` exit 0（UI 1800/1800、0 skip）；`go vet ./...` exit 0；race 覆盖 imagepull+runtime 11 包全 ok、零 DATA RACE（高负载下 docker terminal / nerdctl walk 失败经 1443e0d1f 与 5efef9391 基线 race 复跑甄别为负载时序抖动）；store 包 race 为包级累积时长问题（三次超时各抓不同无辜测试），90m 复跑结果见 `secrets/e2e/final-race-result.json`；UI c8-all exit 0（1800/1800，All files 92.39% 行 / 87.1% 分支 / 85.58% 函数）。L2/L3 不适用：本批产品侧改动仅 `ui/src/pages/models/ModelFilesSection.tsx`（83e820fd1），无 Go 产品改动，调度/部署/agent 运行时/容器行为路径未触及。证据：最终 clone `secrets/e2e/final-*.json`。
- 覆盖率查看页：9876 `gpuworks-e2e-second-batch-coverage-20261022.html`（110 登记条目、14 份最终报告）；第一批报告保持原版本。
- 专用环境 `/tmp/gpuworks-e2e-second-20261021` 已按 owner.json 所有权核验回收：docker ps -a 为空、daemon ID 0826e727 复核后 dockerd/containerd/init/launcher 全停、ram tmpfs 随 namespace 消亡、base 目录与 init 脚本已删；DEV 报告（61 目录）持久化在 `secrets/e2e/dev-reports/`，环境证明与 daemon 日志在 `secrets/e2e/environment/`。
- 交付约定已兑现：独立 commits、每提交独立审查至零新增/零遗留、一个非 Draft MR 不自动合并、最终最新目标 clean build、en/zh、产品变异、适用 Make/race/vet/c8-all 与自有资源回收。
- 任务与独立 grounding 留在本 checkout 的 `.ultraplan/plans/second-*.md`、`task-second-*-spec.md`；实际完成状态登记在 `secrets/e2e/`（mutations/、dev-reports/、environment/）与最终 clone 的 `secrets/e2e/`。

---

# 第一批进度交接（运行前历史快照）

第一批 MR !590 已于 2026-10-21 合并：完整矩阵 12/12 绑定 `46b0dfae2`，最终 source `5ba3fd43` 的终端 en/zh、相关三包 race 和 CI 52631 另行补差。下面保留原运行前快照，不能把旧“待运行”或旧资源状态当作第二批状态；最终第一批交接见其独立 checkout 的 `secrets/e2e/handoff-final.md` 与 9876 报告。

目标：交付批准计划的 S（隔离运行与证据入口）、A（健康检查）、B（实例诊断）、C（容器终端）、
D（HF / ModelScope / 模板快速登记）。各部分分 commit，集中到一个非 Draft MR；每个 commit
由未参与编写的 subagent review，修复并复审直到零新增、零未解决项。

## 当前状态

- 目标分支：`wei/master`，启动基线 `9ae6cde3716e79c85f58c023d4f529169848deda`。
- 汇总分支：`test/e2e-first-batch-20261021`，worktree `../gpuworks-e2e-first-batch-20261021`。
- 唯一普通MR：[!590](http://dev.zstack.io:9080/wei.wang/gpuworks/-/merge_requests/590)，非Draft，不自动合并。
- 本文为最终运行前快照；实际验收状态以文末产物和MR描述为准，DEV与最终结果分开。

| 部分 | 编写位置 / 分支 | 状态 | Commit / review / 运行证据 |
|---|---|---|---|
| S 公共运行入口 | 汇总 worktree | 14 项生命周期 + 4 项产物 + 10 项批次自测通过；生命周期 R1 / 批次 R2 APPROVE | `718e18f4e`、`c0e0bc8be`、`029853c3e`；专用 daemon 与镜像就绪 |
| 指标未知值修复 | 汇总 worktree | RTL red→green，7/7 与类型检查通过，R1 APPROVE | 原 `f402ae138`，集成 `eb5bae6fe`；缺失显示 —，真实零保持 0 |
| A 健康检查 | `../gpuworks-e2e-first-health-20261021` | en/zh DEV PASS；13 Python + 6 Go / race / vet，R7 APPROVE；固定5s产品 mutant 被杀死 | en `20261021T090306Z-14017f67`；zh `20261021T091646Z-0efab325`；mutation `20261021T092414Z-cb9ff522` |
| B 实例诊断 | `../gpuworks-e2e-first-diagnostics-20261021` | en/zh DEV PASS；39 fixture 自测，R4 APPROVE；真实 UI mutant 已杀死 | zh `20261021T093412Z-f7173138`；集成中文按钮修复 `277f98e56` |
| C 容器终端 | 同 B，单独 commit | 30 fixture 自测，测试 R3 APPROVE；原泄漏与权限文案修复后 en/zh DEV PASS；产品 close R3 APPROVE，2 HIGH均已关闭；最终矩阵待运行 | en `20261021T095231Z-6b96c63e`；zh `20261021T095447Z-63622c3d`；execution mutant已杀死；边界修复已审；最终矩阵仍待 |
| D 来源登记 | `../gpuworks-e2e-first-models-20261021` | 3 合同 en/zh DEV 全过，9 自测通过，R2 APPROVE；3 产品 mutant 已杀死 | 集成 `f7f02b3e0`、`3cf91eb38`、`7a7f8e4e4`；恢复后3条英文 controls PASS；最终汇总双语仍待运行 |

## 当前运行资源与评审台账

- 专用 daemon 的所有权、真实 launcher PID、socket、namespace 与状态见
  `/tmp/gpuworks-e2e-first-batch-20261021/owner.json`。bridge=none，独立 data/exec root、PID/mount
  namespace；仅本批 daemon 内操作，host-network 容器按 slot 串行，不修改共享 daemon。
- `gpuworks/fake-engine:test` 从本批源码构建；Alpine 3.20 从已有本地镜像只读导出再加载，未 pull。
- DEV 构建：`secrets/e2e/builds/dev-9ae6cde37/` 中的不可变 server/agent 与摘要；
  仅供诊断，最终集成 clean HEAD 仍须重新构建和全量双语验证。
- S 评审：`/tmp/gpuworks-e2e-first-S-review.md`；批次入口：`/tmp/gpuworks-e2e-first-S-batch-review.md`；
  指标修复：`/tmp/gpuworks-e2e-first-metrics-review.md`。
- A 评审：`/tmp/gpuworks-e2e-first-A-review.md`；D：`/tmp/gpuworks-e2e-first-D-review.md`。
  S / 指标 / A / B / C 测试源码 / D / endpoint / UI 本地化 / 父取消错误规范化均零新增、零遗留。
  C 产品 session-close R3 已关闭2 HIGH；最终 clean-tree 运行证据以文末固定入口为准。
- D 原始失败：其 worktree 的 `secrets/e2e/dev-hub-hf-en-20261021-01/report.json`，
  原因是 fixture 拒绝真实 runId 的大写 T/Z，现由规范化前缀及实际 runId 格式回归处理。
- B 原始失败：其 worktree 的 `secrets/e2e/diagnostics-dev-en-01/report.json`，
  runId `20261021T082706Z-65505b5d`；日志链路已实际运行，指标 API envelope 为 `{items,total}`，
  新 oracle 的错误不作为产品错误；环境清理成功、残留为空。

## 已核实的合同，避免把旧计划当产品规格

- 健康检查是 server 周期性经网关发送的最小 chat；不是 agent 的 `/health` 请求。
  配置修改下一轮生效，不重启容器；验证状态恢复，不虚构自动重启。
- Hub 元数据获取是 best-effort：上游不可用仍允许登记，并记录 `metadata_notice`。
  D 验证降级登记后同一 ID 的元数据刷新恢复；真实参数校验 400 单独证明草稿保留及零落盘。
  不把原计划中的“metadata 503 必须阻止登记”强加给产品。
- 此批只做来源登记；不包括公开下载取消。公开下载当前没有取消 UI，peer 同步另有取消语义。
- fake GPU facts 只用于调度；真实容器、网关、文件、PTY 仍实际执行，不宣称真实 GPU 效果。

## 接下来必须完成的门槛

1. 公共入口自测覆盖 prepare 部分失败、cleanup/close/scan 失败、缺失证据及秘密处理，不能 finally 翻绿。
2. 建立专用 Docker daemon、PID/网络资源所有权与预置镜像；不得使用默认共享 daemon。
3. 各 lane 完整控件/失败表/manifest，提交后逐 commit 独立 review 至收敛。
4. 集成所有 commit；fetch 目标最新 tip，clean tree 重建，运行每条新合同 en/zh。
5. 每条合同至少一次产品层变异，在声明所属断言处失败；恢复后重新验证。
6. 完整 Make 检查、适用 L2/L3/race/vet；明确区分执行与仅编译、跳过。
7. 按 runId 观察清理，记录报告身份和 review tracker，更新此文件，创建并核验唯一 MR。

## 运行与保留约定

共享入口自测：`python3 -B -m unittest discover -s test/e2e -p test_local_business_runner.py`。
覆盖 prepare 部分失败、context close 失败、cleanup 异常与伪成功、server 停止失败、
agent 超时后的定向 kill、秘密扫描失败、失败状态不可翻绿、manifest/source 绑定和共享 daemon 拒绝。

- 实际报告写 `secrets/e2e/<runId>/report.json`，私有 DB/凭据/原始日志留 `private/`，不提交。
- 未得到专用环境及串行 slot 前，子 lane 不启动真实容器或产品浏览器。
- 所有资源台账创建即登记，只有独立观察消失才释放归属；失败保留现场和 scoped 回收入口。
- 具体运行命令随可执行入口落地后补充，不将尚不存在的目标写成已经可运行。

## 批量验收入口与开发证据索引

- `make test-e2e-first-batch E2E_BATCH_DIR=secrets/e2e/first-batch-final`：先构建并跑 fixture，
  再串行运行健康检查、实例诊断、容器终端、HF、ModelScope、模板快速登记各 en/zh。
  B/C 入口现已集成；目前尚未运行完整矩阵。必须独占已声明的专用 Docker daemon。
- 汇总是 `batch-report.json`，只索引各自 `report.json`，不充当任何合同的主证据。
  任一失败立即停止，余项保持 NOT_RUN；旧目录拒绝覆盖，源码/目标分支/二进制发生变化即拒绝通过。
- A runtime model hashes 已放入 `health_proxy.weights_sha256`；源码摘要仍由原 viewer 严格检查。
- D DEV 索引：作者 worktree `secrets/e2e/model-hub-author-handoff.json`。
  中文 HF `20261021T084619Z-cfcb77d5`、MS `20261021T084709Z-84830e85`、quick
  `20261021T084756Z-e117eb2e`，均清理成功、无残留；这些不是最终汇总源码的通过证据。
- D 产品变异：HF 丢弃 browser weight → `20261021T084931Z-7f071ea2` 精确 weight_gib 断言失败；
  MS 刷新后 context=8192 → `20261021T084956Z-5e905818` context_length 断言失败；
  quick 复用确认仅关窗不应用模型 → `20261021T085140Z-f71daddc` ModelPicker 选中断言失败。
  三者清理成功、残留为空，恢复后3条英文 controls 全 PASS；完整索引见作者 worktree
  `secrets/e2e/model-hub-mutations-20261021/mutation-results.json`。
- 初轮完整 `make test` 已完成，日志 `/tmp/gpuworks-e2e-first-batch-make-test-20261021.log`；
  最终冻结源码后仍须重跑受后续产品修复影响的完整检查。
- L2/L3 独立验证环境 `/tmp/gpuworks-e2e-first-validation-20261021/owner.json`：
  单独 net/PID/mount namespace、独立 containerd / Docker / bridge，避免打断浏览器环境；
  镜像只从现有本地缓存只读导出；最终冻结来源的L2/L3待重跑，旧attempt见新增失败段。

## 实际运行暴露的产品缺陷与当前阻塞

- 已修复：未知引擎指标原来显示0，现显示 —，真实零值仍保留。
- 已修复：Docker 容器终端丢失 agent 的 DOCKER_HOST；现在 argv 显式绑定已验证的本地 socket，
  相对路径在 agent cwd 绝对化，避免 terminal 切换 home 后选错 endpoint。
  集成 `50b294aa1`、`0064a56e8`；独立 endpoint R2 APPROVE，整包 race/vet 通过。
- 原始已修复缺陷：关闭浏览器终端后，server audit 已结束且并发计数归零，但 Docker daemon/shim 管理的
  容器 shell 仍为 S，原 PID/start_ticks 不变，并非 zombie。host Docker CLI 退出不等于容器 exec 退出。
  两次真实失败：`20261021T091003Z-b3c8aa17`、`20261021T091317Z-835a7f8d`；各自报告在
  汇总 worktree `secrets/e2e/dev-container-terminal-en-01/`、`dev-container-terminal-en-02/`，
  诊断 raw /proc 见 `secrets/e2e/debug-tools/process-states.jsonl`。测试结束均环境清理成功、残留为空。
- 已集成限定的 runtime session close：仅 Docker 增加 argv+关闭钩子，私有 session nonce /
  PID/start_ticks/SID 所有权与 closed guard；通过相同已验证 daemon 在容器中回收该 session，
  不杀容器或其它 session，不依赖非 root agent 的 host kill 权限。首稿集成 `efdf005fe`，
  后续 `04f0d0464` / `d7da53e51` 修复2 HIGH，独立 R3 APPROVE；最终运行另见文末。
- B 有效变异：`20261021T091531Z-c8a0c066`，独立 engine/TSDB 为0.375，产品 UI 变异后显示47，
  精确 expected37 断言 FAIL，清理成功/无残留。此前 `090835Z-52e01b5e` 是 popup 定位失败，
  未到目标断言，明确不计为 mutation kill。变异 patch / binary / UI hash 在独立 worktree
  `../gpuworks-e2e-first-diagnostics-mut-20261021/secrets/e2e/mutations/diagnostics-ui-gauge/`，产品源已恢复。
- C 新session版 echo-only mutant 已被目标execution断言杀死，详见最新快照；旧98de变异不作为依据。
- B / C review：`/tmp/gpuworks-e2e-first-B-review.md`、`/tmp/gpuworks-e2e-first-C-review.md`；
  endpoint：`/tmp/gpuworks-e2e-first-terminal-endpoint-review.md`；产品回收：
  `/tmp/gpuworks-e2e-first-terminal-session-review.md`，R3已收敛，最终运行见固定入口。
- L2/L3 独立环境真实 Alpine 容器已验证可运行；C已释放，环境准备不等于 suite 通过。
  L2使用第三套专用环境，L3使用validation环境，最终运行状态分别记录。

## 最新验证快照与剩余交付门槛

- 冻结前目标master `b41a3b7a9163bd555c214e4e8c680c6fba94584a` 通过 `585300f52` 合入，
  仅5条上游image-inspect脱敏变动，独立master R3逐blob核对通过。前次777/482合并也分别已审。
- 第一轮完整 `make test` 已退出0：Go检查186包次（182个不同包路径，包含重复tag校验）、1746个UI测试通过，UI零失败/零跳过。
  日志 `/tmp/gpuworks-e2e-first-batch-make-test-20261021.log`。它开始于较早基线，
  后续 session-close / 取消 / UI权限修复须在最终冻结源码上补齐完整检查，不能冒充最终通过。
- UI 权限提示原错误：实际403/code正确，却显示backend英文fallback。独立 RTL 先红后绿，
  en/zh 通过真实 Response → APIError → 精确本地化警告，零WS、无containerref泄漏；
  产品修复集成 `58d76eed0`，独立 R1 APPROVE。
- 连接取消竞态：原分支亲测20/1000返回stream reset/context canceled错误组合；held parent传播
  回归确定性证伪。`2a89225a2` 修正父取消优先级，独立1000次控制/race/vet与复审通过。
- C close 首稿的审查阻塞（`/tmp/gpuworks-e2e-first-terminal-session-review.md`）：
  1. 强杀子进程后立即强杀leader，容器PID1不reap时可能留下Z，必须先等待父进程reap再逐层回收。
  2. `TermExit(0)` 早于remote cleanup，后续cleanup失败的第二个exit已无法被server/browser看到。
  两者都有离线真实进程/协议回归。`04f0d0464` 等待父进程逐层reap；`d7da53e51` 在唯一exit前
  执行exactly-once cleanup并传播失败。独立 R3 零新增、零遗留；四个相关包完整race/vet通过，
  原wire复现当前首次唯一[-1]，回退后[0,-1]；host nil-hook行为保留。
- C 正常有限路径 en/zh 已通过（包括三种退出及readonly403），所有清理成功、残留为空；
  这两条DEV早于最终边界修复，不能替代最终矩阵。新session版 echo-only mutant在 `20261021T100129Z-a9b954a3`
  的assembled marker断言正确FAIL：真实PTY只有输入回显，完整运行输出缺失；清理成功且残留为空。
  它的normal-agent与三退出正控二进制逐字节一致；mutation patch/overlay/两二进制hash见作者
  `secrets/e2e/terminal-echo-only-session-mutant-f5dec2c0b/metadata.json`，baseline f5dec，
  不把这个execution mutation当作cleanup边界的证明；边界以独立进程/协议回归和最终运行验证。
- A 完整索引：作者 `secrets/e2e/health-author-handoff.json`，含历史FAIL与额外scoped reclaim、
  en432.7秒/zh439.2秒PASS、固定5s mutant在target-probe-slow-timeout正确FAIL；最后三次均清理成功。
- 第三套专用 L2 环境：`/tmp/gpuworks-e2e-first-l2-20261021/ready.json`，独立net/PID/mount、
  containerd和Docker，只装本地Alpine/fake-engine，约37MB，smoke容器已删除。最终冻结来源的L2待重跑；旧中止attempt见末段。
## 冻结源码后的验收入口与最终状态

最终验证目录为独立Git仓库 `../gpuworks-e2e-first-final-20261021`，分支名称仍相同；
它隔离其它会话fetch引起的共享remote-ref漂移，不修改严格身份校验。原汇总worktree保留早期证据。
本文在最终运行前提交，以便12条报告绑定同一干净HEAD；不预填最终PASS。实际状态读取以下
固定产物，保留每条合同的主报告，不用汇总代替主证据。失败保留，重新运行必须使用新目录，禁止重试翻绿。

| 证据 | 固定位置 | 说明 |
|---|---|---|
| 总交接状态 | `secrets/e2e/final-verification.json` | exact source/target、构建摘要、检查退出码、MR、清理结果；运行后更新，不提交秘密 |
| 12条双语矩阵 | `secrets/e2e/first-batch-final/batch-report.json` | 各lane/locale独立report、runId、cleanup、scan；索引不替代主报告 |
| 构建 | `/tmp/gpuworks-e2e-first-final-build-20261021.log` | clean HEAD完整`make build`，server内嵌同源UI/跨平台agent |
| 完整检查 | `/tmp/gpuworks-e2e-first-final-make-test-20261021.log` | `make test`；架构/时间/i18n等检查；UI覆盖统计另列 |
| UI全源码覆盖 | `/tmp/gpuworks-e2e-first-final-ui-coverage-20261021.log` | `cd ui && npm run test:coverage`，独立于普通`npm test`，使用`c8 --all` |
| L2 | `/tmp/gpuworks-e2e-first-l2-20261021/l2-result.json` | 真实`make test-l2`，strict模式，列出skip/fail；准备就绪不算通过 |
| L3 | `/tmp/gpuworks-e2e-first-validation-20261021/l3-result.json` | 真实`make test-l3`，strict模式与本地registry，列出skip/fail |
| race/vet | `/tmp/gpuworks-e2e-first-final-race-vet-20261021.log` | terminal/conn/docker/app受影响包与`make vet` |
| 环境回收 | 三个专用环境目录内`cleanup.json` | 校验daemon/PID/namespace所有权后清理，观察残留，绝不全局prune |

最终执行顺序：确认独立review零遗留 → 本文提交并复审 → clean HEAD构建 → UI构建静止后
完整Make、L2/L3、串行浏览器矩阵 → 三套专用环境回收 → 仅汇总分支推送一个非Draft MR。
MR创建后链接与实际结果写入final-verification和MR描述；不自动合并。

覆盖边界仍保留：C仅验证浏览器context销毁导致的真实WS断开，不声称任意网络故障重连；
D仅来源登记/刷新，不声称公开下载取消；本批不代表全产品业务或组件控件100%覆盖。

## 验证中新增的失败与修正

- 原汇总目录的 `secrets/e2e/first-batch-final/batch-report.json`：英文health真实PASS
  `20261021T102638Z-506de815`，随后另一会话更新共享wei/master，严格入口停止，余项NOT_RUN；
  源码/二进制仍一致的独立观察见同目录 `target-drift-proof.json`，不把这轮索引称作最终PASS。
- 首轮MR CI52600，core-l3 job246023：新增PTY用例要求Unix socket，而CI原传TCP endpoint；
  `e27dcd897` 给DinD/Go共享job-owned短Unix socket、检查ready并仅chown该socket。
  `c4fccc078` 让真实终端测试复用已配置shell镜像，避免忽略CI缓存而另拉镜像。
  两补丁独立CI R2 APPROVE，实际新pipeline结果另记录，不能用静态检查冒充通过。
- a737全源码c8：1754项，1753通过/1失败/0跳过，statement/line 92.18%、branch86.84%；
  失败为UpdatesCard加载遮罩尚未解除就点击Upgrade，保留原FAIL，不将统计百分比当测试全过。
  原始V8 JSON逐文件校验后压缩留在原汇总目录 `secrets/e2e/ui-coverage-raw-a737e575f.tar.gz`，
  proof记录515文件/摘要；只释放已验证归档的自有临时数据，未删除未知缓存。
- a737 L2被宿主空间低于1.5GiB保护门槛中止，无产品通过结论；报告原样保留。
  脱离进程组的两个测试进程经身份复核和pidfd定向TERM，子已消失、父Z明确不算gone，
  最终仍须观察namespace停止后真实消失。L2重跑使用新attempt产物且不覆盖该失败。
- a737 L3实际130 PASS/1 SKIP，跳过的TestDockerInspectL3缺少GPUWORKS_TEST_IMAGE；
  后续完整运行显式绑定已缓存本地镜像再核跳过数。首轮sudo PATH丢失的启动失败另留attempt01。
- 三套cleanup脚本均经独立复审：fresh inventory须重新检查活跃suite/client/FD/资源；
  PID复用、host namespace、socket与daemon身份校验保留，Z不能算资源消失。只读批准不等于已回收。

- a737完整Make还在 `TestSidecarPlanRespectsCumulativeBudget` 失败：测试真实查询物理临时盘，
  2GiB层按factor3计费；当时剩余容量不满足其“大层优先且至少两层”的前置条件。
  保留FAIL，不改无关产品预算逻辑。最终Go检查使用专属可执行tmpfs `/tmp/g21`
  （16GiB逻辑容量、按需占用），身份/回收记录 `/tmp/gpuworks-e2e-first-unit-tmp-20261021.json`；
  同一失败case在足量临时盘上已实际验证，最终完整Make仍另跑。
  c8原始临时JSON另用本批私有内存目录，报告完成后校验归档，避免再次挤占物理盘。

- UpdatesCard测试修复（作者 `0bfc470f8` / `1869d9af2`）已独立R2 APPROVE，0新增/0遗留：
  同步readiness predicate先拒绝Checking/遮罩/disabled状态，held请求释放前能确定性反证旧判断，
  恢复后仍真实userEvent点击，保留确认、精确升级调用、健康版本及刷新计数。
  作者普通/focused c8各17/17，独立两项相关case及负对照通过；不替代最终全量c8。
  评审 `/tmp/gpuworks-e2e-first-updates-card-review.md`，未修改生产UI或关闭pointer检查。

- 新CI52614的真实终端runtime包已通过（job246101，5.844秒），原Unix endpoint阻塞已解除；
  同轮docs-build job246096触发既有测试夹具缺README，作者 `f417a9ebd` 仅补合法索引和how-to页面，
  未改生产stage_docs/CI规则；独立docs-assets R1的12/12测试通过、0新增/0遗留。
  原pipeline失败保留；最终新HEAD的pipeline与本地完整验收仍以最终index/MR为准。
