# 浏览器 E2E

编写前阅读 [主规范](browser-e2e-testing-standard.md)、[组件清单](browser-e2e-component-checklists.md)
和 [agent 入口](AGENTS.md)。当前使用 Python Playwright 与系统 Chrome。

业务范围以 case 的名称、Mainline、步骤和断言为准；[coverage/](coverage/)关联真实用例、字段合同、
组件维度和缺口。本 README 只维护通用运行约定，不重复逐用例流程或保存通过数。
环境能力与后续候选维度见[单机盘点](single-machine-coverage.md)。

## 查看覆盖与缺口

运行 `make test-e2e-coverage`，然后在浏览器打开 `secrets/e2e/coverage/index.html`，即可按业务模块、
组件维度、环境和语言查看合同、规范项、枚举分子/分母、未覆盖项与运行报告。
工具只读取现有清单和报告，不运行测试。组件百分比表示清单声明覆盖，运行结果另列；
分母未知、未登记模块、历史报告和证据不足均保留，不计为当前版本通过。

默认读取当前 checkout 的 `secrets/e2e/`；可用
`make test-e2e-coverage E2E_REPORT_DIR=/path/to/artifacts` 指向其它报告目录。
多个报告来源、单份报告和额外二进制身份核对见 `python3 scripts/e2e_coverage.py --help`。
生成页不包含报告中的请求、响应、日志或凭据，但仍保存在 gitignored 目录，按本机测试产物管理。
工具离线自测使用 `make test-e2e-coverage-check`；已安装 Chrome/Playwright 时可另跑
`python3 -B scripts/e2e_coverage_browser_check.py`，用合成视图数据验证真实浏览器交互，不计为产品 E2E 通过。

## 本地执行

需要已有 Python `playwright` 包及系统 Google Chrome（`channel="chrome"`）；Go/Node 构建依赖
按[开发环境](../../docs/dev/setup.md)配置。入口不自动安装依赖，不复用已有服务或浏览器 storage state。

```bash
make build
make test-e2e-local

# 查看当前 Make 入口；聚合套件并不自动包含所有定向 case
rg -n '^test-e2e-' Makefile test/e2e -g Makefile -g '*.mk'
# 根据 coverage 中的 test_file / command 和入口文件头选择 case，再查看其实际参数
python3 test/e2e/network_storage_lifecycle.py --help
# 示例：已构建后的单条中文运行
NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost \
  python3 test/e2e/network_storage_lifecycle.py --case storage --locale zh

python3 -m unittest discover -s test/e2e -p 'test_*.py'
```

各本地 Make 入口会构建二进制；直接调用脚本时先构建。专用依赖、语言、参数组合与单位测试命令
写在各入口文件头，不能假设所有脚本具有同一 CLI。`test-e2e-local` 的选择集合以 Makefile 及其加载的 `.mk` 文件为准，
单条入口存在不代表已进入该集合。

本地隔离入口使用独立 server、SQLite 和 runId，端口按 8800、8802…分配并加进程锁。
bootstrap 为 admin/password，只绑定 127.0.0.1，不修改已有服务。隔离 server 以 `private/server.yaml` 启动：唯一的存储根目录是 `private/storage`（`LocalServer.storage_root`），存储路径一律放在其下；出站私网例外只放行 `127.0.0.1` 上的本地 fixture。支持 `--server-bin`、
`--artifact-dir` 的入口需显式指定本次构建和全新目录；执行前核对 `--help`。

每条 case 单独报告，失败无自动重试。退出 0 只证明本次所选范围；依赖缺失、断言、清理或产物
扫描失败均应非零退出。`make test`/RTL 不替代浏览器验证；当前未接通用浏览器 CI 或全站覆盖率门禁。

## 远端环境

远端入口必须显式选择服务、主体和资源，不回退到 localhost、自动选机或自动下载。
[remote_runtime_acceptance.py](remote_runtime_acceptance.py) 文件头维护专用配置与前提；按其
`--help` 选择场景。真实地址、凭据和资源 ID 放 gitignored `secrets/`，文件权限 `0600`。

远端内嵌 UI 与本地主线 UI 连接远端是两种独立 lane。后者通过
[remote_ui_proxy.mjs](remote_ui_proxy.mjs)转发真实 API/WS，不更新远端安装，也不替换管理响应；
配置需记录 `ui_base_url` 和 `ui_source_sha`。使用 shell 代理时显式排除本地回环地址。
已知产品阻塞见[远端缺口](coverage/remote-runtime-known-gaps.md)。

执行前核对真实 agent/运行时能力与共享负载；临时 agent 的安装、隔离和移除由环境准备流程负责，
浏览器 runner 不自动执行 SSH 安装。不得在共享 GPU、容器或文件系统上使用无范围的清理操作。
不同环境、身份和适用集合分别统计，不能用 API 准备资源代表相应 UI 操作已验证。

## 报告、秘密与清理

产物默认保存在 `secrets/e2e/<runId>/`：`report.json`、脱敏 trace、截图和服务日志。
报告记录源码 SHA/dirty 状态、源码与二进制摘要、环境、实际证据及清理结果；代码或构建变化后，
旧报告只归属原版本。清单中的目标或说明不是执行证据。

登录和一次性密钥显示/复制期间关闭 trace；其它产物按入口脱敏并扫描。`private/` 中的数据库和
原始日志仅供本机诊断，禁止上传。失败不立即删除现场；本地入口停止自有 server，执行者在诊断后
七日内按 runId 清理。远端失败保留 owned ledger，先诊断再按其范围恢复。

成功必须完成该 case 声明的自有资源与进程清理；既有模型、镜像、存储、缓存及其它工作负载不归
测试所有，禁止全局删除。产品没有历史删除 API 的记录按产品保留策略处理，并在报告列明。
