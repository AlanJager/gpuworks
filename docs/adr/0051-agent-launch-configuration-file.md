# ADR 0051: agent 的启动配置记在 `<state>/gpuworks-agent.conf`，所有启动方式都是 `run --state`

日期：2026-10-19。状态：已接受。

## 背景

agent 的启动配置（server 地址、节点 token、连接方向、监听地址、运行时 driver、mount root、udocker 离线包）原来散落在每种启动方式自己的命令行或环境里：systemd 单元的 `ExecStart` 带 `--server --token --connect-mode --listen --state`，nohup 的 `agent-restart.sh` 固化一份环境和参数，launchd 的 plist 把 token 放进 `EnvironmentVariables`，手工 `upgrade` 再拼一遍参数让新进程重启。后果：

- 同一件事有四五份拼写，改一个选项（例如 server_dial 的监听地址）要同时改几处，默认值也在 agent、安装脚本、API、各云厂商驱动里各写一份（`:19090`）。
- token 出现在 `ps`、`systemctl cat` 和单元文件里，所有本机用户可读。
- 所有要“看主机上的 agent 属于哪个 server / 哪个 token”的代码（安装脚本的替换警告、预检查的 `Existing agent`、卸载时按 token 认定要停的 agent、带外恢复选 systemd 还是 nohup）都靠解析各种启动方式的命令行，每处一套 grep。
- `server_dial` 的 agent 从不连接 server，却被要求带 `--server`，于是产生了一个没有意义的必填项。

## 决策

1. **配置只记在一个文件里**：state 目录下的 `gpuworks-agent.conf`，权限 `0600`，`KEY=value` 逐行（`SERVER`、`TOKEN`、`CONNECT_MODE`、`LISTEN`、`RUNTIME_DRIVER`、`MOUNT_ROOT`、`UDOCKER_BUNDLE`），值按字面读，不认识的键忽略。字段、校验、读写和默认值（`agentconf.DefaultListen` = `0.0.0.0:18800`）都由叶子包 `internal/agentconf` 定义一次，agent、server（校验要交给安装脚本的值）和安装脚本共用。
2. **只有一个写入方**：`gpuworks-agent configure --state DIR …`，token 从标准输入读，先写临时文件再改名，整份替换。安装脚本、SSH 向导、离线包都经安装脚本调用它。
3. **每种启动方式都只运行 `gpuworks-agent run --state DIR`**：systemd 单元的 `ExecStart`、nohup 与 `agent-restart.sh`、launchd 的 `ProgramArguments`、手工升级后的重启。token 不再出现在任何命令行、单元或 plist 里。
4. **取值优先级**（`run`、`smoke`、`upgrade` 相同）：命令行 flag > 环境变量 > 配置文件 > 默认值。flag 与环境变量保留，用于临时覆盖和测试；它们存在于文件之前，所以排在文件之上。
5. **文件不存在不是错误**：较早安装的 agent 没有这份文件，继续从自己的 flag 和环境运行，升级二进制不改变它们的启动方式；重新执行一次安装才迁移到文件。
6. **`server_dial` 的 agent 不涉及 server 地址**：不带 `--server`，API 不为这类节点保存 `server_url`（不论控制台发来什么），`run` 只需要 token 和监听地址；手工 `upgrade` 无从下载，直接拒绝并说明由 server 推送。
7. **卸载一并删除这份文件**（里面有 token）。

## 兼容规则

识别主机上 agent 的读取方在较长时间内要同时面对两种 agent：有配置文件的，和较早安装、参数仍在命令行里的。所以：

- 读取方两种形态都接受：先读命令行里 `--state` 指向的目录里的配置文件，没有文件再退到命令行的 `--server`、`--token`、`--connect-mode`、`--listen`。shell 一侧只有一份实现，`agentconf.ShellFuncs`（`gw_conf`、`gw_line_value`、`gw_unit_value`、`gw_unit_state`），安装脚本、预检查、卸载检查、探活都内联同一份，不各自解析命令行。Go 一侧同理：`run`/`smoke`/`upgrade` 的 flag 仍然有效，`upgrade` 对没有文件的 agent 仍带完整参数重启，`launchd.ReadJob` 仍读得出旧 plist。
- 这些“按命令行读”的代码只为旧 agent 存在。**删除它们（`agentconf.ShellFuncs` 的命令行分支、`launchd.ReadJob` 对旧 plist 的解析、`upgrade` 对没有文件的 agent 带完整参数的形式）的条件是**：server 仍要与之通信的最老的 agent 版本，新于引入这份配置文件的那个版本（包含提交 “Record the agent's launch configuration in `<state>/gpuworks-agent.conf`” 的第一个发行版，以 `gpuworks-agent version` 报告的发行版本号为准）。换句话说，不再支持引入文件之前的 agent 之后，才可以删。
- 目前 server 与 agent 之间没有强制的最低版本检查（见[升级与版本兼容](../how-to/upgrade-and-compatibility.md)），所以“最老的仍被支持的 agent 版本”还没有一个机器可读的值：要删除之前，先引入这个下限并写明它，再让上面的条件成立；在那之前一律保留两种形态。已有的老 agent 即使不重装也继续工作，只是 token 仍在其命令行里，直到重新执行安装。

## 权衡与后果

- 多了一个有 token 的文件：权限 `0600`，与 `identity.json` 同在 state 目录，由卸载删除。换来 token 离开 `ps`、单元文件和 plist，这是净收益。
- 改配置要重新执行安装（或 `configure`）再重启 agent，不能再改单元文件。这是有意的：只有一处可写。
- 启动配置不进数据库、不在 server 侧保存副本：节点的 `connect_mode`、`server_dial_listen` 等事实本来就在节点记录里，文件只是让节点自己启动时不必再向任何人要。
- `--data-lane`、`--dcgm` 不进文件：前者是排障时的开关，后者已有环境变量，都不是安装时的决定。

## 未采用方案

- **继续把配置放在 argv 与环境里，只把拼写集中到一个函数**：解决了拼写重复，没有解决 token 暴露，也没有解决“读别人命令行”这件事；解析方仍要面对单元、plist、脚本三种格式。
- **每个二进制旁放一个 sidecar 文件（`<install-dir>/gpuworks-agent.conf`）**：同一台主机上可以有多个 agent（nohup 模式、不同安装目录各一个），一个安装目录一份配置在语义上也说得通，但 agent 已经以 state 目录为身份（`identity.json`、日志、模型、PID 都在那里，`uninstall` 与 `runveil` 也按 state 找），配置跟二进制走会出现“同一个 state 目录被两个二进制用出两份配置”的歧义，升级替换二进制时也要考虑文件是否一起搬。跟 state 走与现有的身份和清理边界一致。
