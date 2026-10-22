# 安装和升级控制面

**适用场景**：首次在一台 Linux 主机或 Apple silicon Mac 上安装 GPUWorks 控制面（server），或对已装好的控制面做升级、重复安装修复、卸载和排查。

**前置条件**：

- 一台 linux-amd64 主机或 Apple silicon Mac（macOS 15+，见 [macOS](#macos)），能拿到发布包（主机不能访问发布源时，把发布包拷过去即可，升级见[离线升级](#离线升级)）。
- systemd 模式需要 root 或 sudo；没有 sudo 时改用 `--no-systemd` 装到用户目录，见 [systemd 与 nohup](#systemd-与-nohup)。
- 装好后接入节点、部署第一个模型，见[从零启动并部署第一个模型](../tutorials/getting-started.md)。

控制面是单个静态二进制。`gpuworks install` 把它自己拷到安装目录、写 service、启动并等到
`/healthz` 应答；`gpuworks upgrade` 从发布通道取当前版本，备份后原地替换，探活失败自动回滚。
不需要手抄 systemd 单元，也不需要在升级前手工备份。默认值、目录布局、文件权限和凭证文件等查表内容见[控制面安装布局](../reference/install-layout.md)，全部参数见 [CLI 与安装脚本](../reference/cli.md#gpuworks-install)。

## 安装

在目标主机上下载发布包，校验解压后执行。发布包放在三个内容相同的发布源上：中国大陆用阿里云 OSS
`https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com` 或华为云 OBS `https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com`，
其他地区用 `https://dl.gpuworks.ai`。`<channel>/latest` 是该渠道当前的 release id：`release`（正式发布版）是打了版本号的版本，
`nightly`（滚动预览版）是每次 master 构建：

```bash
src=https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com     # 或另外两个源
id=$(curl -fsSL $src/nightly/latest)
curl -fLO $src/releases/$id/gpuworks-$id-linux-amd64.tar.gz
curl -fLO $src/releases/$id/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf gpuworks-$id-linux-amd64.tar.gz
cd gpuworks-$id-linux-amd64
./gpuworks install
```

内网也可以从发布站 `https://gpuworks.zstack.ai/downloads/` 下载同一批发布包。

install 以操作员身份运行，只有需要特权的少数步骤（建 `/opt` 目录、建服务账户、写 systemd 单元、`systemctl`）
以单独的 `sudo` 命令执行，`sudo` 会在第一次特权命令时问一次密码。随后打印 banner 并逐个提问，**回车即采用括号里的默认值**：

```
Press Enter to take the default in brackets, or type your own value and press Enter.
install directory (default: /opt/gpuworks/server):
listen address (default: 192.0.2.10:8800):
start the service automatically (default: yes)? [yes/no]:
Admin password (default: random):            ← 仅全新安装
```

- 监听地址默认取本机默认路由出口 IP、端口 8800；只输入端口号会保留探测到的地址、只替换端口。
- 命令行已给出的参数（`--dir`、`--addr`、`--no-systemd`、`--password`）不再提问；`--quiet` 或 stdin 不是终端时不打印 banner、不提问，全部按默认值走。
- 已有安装时，默认值是 `server.yaml` 里记录的现值，回车等于保持原状；数据库里已有 admin 账户时跳过密码提问。
- 命令结束时打印访问地址、服务方式、数据/日志/备份目录和管理员凭证。
- 各提问的默认值与跳过条件见[控制面安装布局 · 交互提问](../reference/install-layout.md#install-的交互提问)。

控制面不存储也不推断自己的对外地址（见 [ADR 0025](../adr/0025-server-holds-no-external-address.md)）：
添加节点等 API 要求调用方显式给出 `server_url`，给节点或用户一个域名或公网入口见[给控制台一个公网地址](expose-public-address.md)。

## 验证与首次登录

1. 检查服务已就绪：`curl -fsS http://<监听地址>/healthz`。
2. 浏览器打开摘要里的访问地址，用用户名 `admin` 和摘要里打印的密码登录。
3. 登录后修改密码并删除 `<data>/initial-credentials`：它保存的是明文，只用于交付初始密码。
4. 忘记密码时执行 `gpuworks reset-password --password NEW --data <data目录>`（systemd 模式下需要操作员已在 `gpuworks` 组，见下）。

密码的选择优先级与 `initial-credentials` 格式见[控制面安装布局 · 凭证文件](../reference/install-layout.md#凭证文件)。

## systemd 与 nohup

有可用 root/sudo、`systemctl` 存在且 `/run/systemd/system` 真的存在时写 systemd 单元；否则回退到
nohup 后台进程 + PID 文件。`--no-systemd` 可以强制回退。两种方式都只靠 `server.yaml` 启动：systemd 单元执行
`serve --config <dir>/server.yaml`；nohup 模式在它前面多一层 `serve --supervise`，由这个父进程充当 systemd 的
`Restart=always`：server 退出（崩溃、在控制台升级或重启）3 秒后用安装目录里的二进制重新拉起，收到 SIGTERM/SIGINT
时转给 server、等它退出后自己退出；父进程被 SIGKILL 时 server 跟着被杀，不会留下占着端口的孤儿。`gpuworks.pid`
记的是这个父进程。

systemd 模式下 install 会把操作员加入 `gpuworks` 组（摘要里会提示），之后 `reset-password` 和升级时的文件写入都不需要 sudo。**当前终端**
不会立刻拿到新组，请 `newgrp gpuworks` 或重新登录后再跑 upgrade/reset-password，否则会报权限错误。

nohup 模式没有特权步骤：进程以调用者身份运行，唯一例外是首次创建 `/opt` 目录。sudo 也不可用的精简环境里
install 会拒绝写 `/opt` 并提示改用用户级目录：

```bash
./gpuworks install --dir ~/gpuworks-server --no-systemd
```

nohup 模式没有任何东西会在重启机器或重启容器后拉起它，安装摘要里会写明；重新拉起用
`gpuworks restart --dir <安装目录>`（会先结束旧进程再重新执行 `serve --supervise --config`，并等待 `/healthz` 恢复）。
nohup 进程归安装者：root 装的只有 root 能重启。

### macOS

Mac 上用同一个 `gpuworks install`（发布包 `gpuworks-<release_id>-darwin-arm64.tar.gz`）。有 `sudo` 时（Mac 的管理员账户都有）
它写一个 system 域的 launch daemon `/Library/LaunchDaemons/ai.gpuworks.server.plist`：**以执行安装的用户运行**（不是 root，
不建专用账户，`--service-user` 被忽略），开机即起，不需要登录；server 退出后由 launchd 拉起，因此控制台升级、重启照常。
默认目录同样是 `/opt/gpuworks/server`（首次创建 `/opt/gpuworks` 走 `sudo`，之后整棵树归该用户）。

```bash
./gpuworks install          # 问 sudo 密码一次
gpuworks restart            # launchctl 重新加载 plist
gpuworks stop               # 停服；plist 留着，下次开机照常起来
```

同一台 Mac 再装 agent 时，agent 在 `/opt/gpuworks`，与控制面的 `/opt/gpuworks/server` 互不覆盖。
`--no-systemd` 在 Mac 上同样表示不用服务管理器，改为 nohup 后台进程。plist 的内容见[控制面安装布局](../reference/install-layout.md#launchd-plistmacos)。

### 在容器里运行

容器里通常没有 systemd，install 会走 nohup 模式。安装目录（二进制、`server.yaml`、数据目录）必须放在持久化存储上：
控制台升级是原地替换安装目录里的二进制，镜像里另带的二进制不会被升级，容器按旧镜像重建后跑的就是旧版本。

- **能决定容器的启动命令时**（自己的 `docker run`、compose、平台的启动命令），把它设为前台运行的守护进程：

  ```bash
  /opt/gpuworks/server/gpuworks serve --supervise --config /opt/gpuworks/server/server.yaml
  ```

  它作为容器的 1 号进程会回收孤儿进程，`docker stop` 的 SIGTERM 会转给 server 并在其退出后以 0 退出；容器重启后
  server 自动回来，控制台升级与回滚照常。路径必须是安装目录里的二进制（`install` 摘要里打印了这条命令）。
  这种形态下不要在容器里执行 `gpuworks restart` 或 `gpuworks upgrade`：停掉 1 号进程就是停掉容器，二者会直接拒绝
  并提示改用控制台升级或重启容器；用 `docker run --init` 时守护进程不是 1 号进程，命令不会拒绝，但停掉它同样会让容器退出。
- **启动命令由平台决定时**（租用的 GPU 容器），在容器里执行 install 即可；容器没重启期间控制台升级与崩溃重启都正常，
  容器重启后用平台提供的开机脚本或手工执行 `gpuworks restart --dir <安装目录>` 拉起。开机脚本里要后台启动、不能阻塞：
  `setsid nohup <dir>/gpuworks serve --supervise --config <dir>/server.yaml >/dev/null 2>&1 &`。实测可用的位置：
  AutoDL 是自建的 `/etc/autodl.sh`（每次开机执行）；Vast.ai 是 `/root/onstart.sh`；RunPod 可以把启动命令改成先起 sshd、
  再 `exec` 上面的守护进程（修改启动命令会重置 pod，并清空容器盘）。注意哪些目录在重启后保留：RunPod 停止再启动会清空容器盘，
  只有 `/workspace`（网络卷）保留。
- Kubernetes 每次重启容器都从镜像重建文件系统，不适合控制台升级；在那里请通过更换镜像升级。

## 重复执行 install

再次执行 install 是修复路径，不是重装：保留既有 `server.yaml` 的值（命令行显式给出对应参数才覆盖），替换二进制、
重建 service、重启并探活，但绝不触碰已有的数据库、`master.key` 和 admin 账户。也可以用来切换服务方式
（例如从 nohup 切到 systemd）。`--password` 此时会被忽略并提示改用 `gpuworks reset-password`。

## 升级

```bash
gpuworks upgrade            # 升级到所选渠道（server.yaml 的 update_channel，默认跟随构建）的当前版本
gpuworks upgrade --check    # 只看有没有新版，不改动任何东西
gpuworks upgrade --base-url https://gpuworks.zstack.ai        # 内网发布站
gpuworks upgrade --source https://mirror.example.com/gpuworks # 自建的发布通道镜像
gpuworks upgrade --archive ./gpuworks-<release_id>-linux-amd64.tar.gz   # 本地发布包，见下
```

流程：同时请求三个发布源的签名索引（`<channel>/index.json`），用第一个校验通过的；按当前平台选
`gpuworks-<release_id>-<os>-<arch>.tar.gz`，从该源下载（失败或 30 秒没有新数据就换源续传），按索引核对大小与 sha256，
停服，打包备份，原子替换二进制，重启，探活（60 秒）。探活失败时自动还原备份、重启旧版本，以非零码退出，
同时打印失败原因和备份路径。整个过程用 `<dir>/.upgrade.lock` 互斥，并发升级会直接失败。
不给 `--dir` 时升级执行的这个二进制所属的安装，同机有多套安装时用哪个目录下的 `gpuworks` 就升级哪一套；
停服后监听地址仍被别的进程占着（例如另一套安装用了同一端口）时直接失败并报出占用进程，不做任何修改。常用参数：
`--channel`（`nightly` / `release`）、`--version ID`（期望的 release id，与渠道当前版本不一致时报错）、`--keep N`（备份组数）、
`--force`（允许替换本地构建）、`--quiet`（无人值守），完整列表见 [CLI 参考](../reference/cli.md#gpuworks-upgrade)。

更新渠道在设置页「版本与更新」里选（正式发布版 / 滚动预览版），保存在 `server.yaml` 的 `update_channel`，命令行与控制台读同一个设置。
渠道里的版本比当前构建旧（例如从滚动预览版切到正式发布版）时不会降级，需要时加 `--force`。

自建镜像：把任一发布源上的 `<channel>/index.json` 与它引用的 `releases/<release_id>/` 原样拷到自己的 HTTP 服务下即可，
索引的签名保证镜像内容没有被改动。

- `--dir` 下没有 `server.yaml`、但有 `server.yaml` 之前的版本留下的 `install.json` 时（老版本的 upgrade 只换二进制、不动这套布局），先把其中的监听地址、数据/日志目录、日志轮转和可信代理写成 `server.yaml`，把用环境变量传配置的单元改写成 `serve --config`，删掉 `install.json`，再照常升级；回滚还原的是同一个二进制，同样从 `server.yaml` 启动。这一步只能由已安装的二进制执行（`<dir>/gpuworks upgrade`），换别的二进制会报错退出、不做任何修改；`install` 与 `restart` 遇到这种安装同样先转写。
- 两者都没有时视为未采纳的安装，报错「no gpuworks installation found at <dir>; run `gpuworks install --dir <dir>` first to adopt it」，不做任何修改。
- 已是最新版时打印“已是最新”并以 0 退出，可以放进定时任务；本地构建（`dev` 或 `-dirty`）不加 `--force` 会拒绝升级。
- 升级只重启控制面：节点上的推理容器和模型不受影响，`/v1` 网关会短暂 503。`/dl/` 里的新 agent 只对新接入的节点生效，既有节点不会自动升级，见[接入节点、升级与卸载 agent](connect-nodes.md)。
- 回滚还原的是升级前快照，升级窗口（约 1 分钟）内的数据变更会丢——此时服务刚重启，几乎没有写入。升级失败后如仍未起来，用备份里的 `bin/gpuworks` 和 `data/` 手工还原。
- 备份内容、保留策略和每次安装/升级追加的审计记录见[控制面安装布局 · 探活与备份](../reference/install-layout.md#探活与备份)。

### 在控制台升级

设置页「版本与更新」显示服务端当前版本与所选渠道的最新版本；「自动检测新版本」开着时（默认），打开登录页 server 就开始检查（登录页不显示结果），并随检查向发布方报告节点数、GPU 型号分布和 agent 版本分布（不含主机名、地址、用户或部署，内容见[配置](../reference/configuration.md#镜像目录与镜像拉取)）；关掉这个开关（`server.yaml` 的 `auto_check_updates: false`）后不再自动检查，也不报告，要查更新就点「检查更新」，登录后有新版本时页头出现「有可用更新」。切换渠道后页面立即显示新渠道并重新检查。读不到渠道时分三种提示：渠道还没有发布任何版本（源都能访问，例如还没有正式版本时的正式发布版渠道）、发布源返回的内容没有通过签名校验（多为需要登录的网关或代理拦截）、连不上发布源（检查出网与代理）。服务端由 systemd 或 `serve --supervise` 托管、运行的是安装目录里的二进制、
且服务账户能写安装目录时（`gpuworks install` 装出来的默认就是这样），可以直接在这里升级：

- **升级**：服务端从发布通道下载本平台的压缩包（按签名索引校验 sha256）；
- **上传升级包**：上传一个 `gpuworks-<release_id>-linux-<arch>.tar.gz`，用于访问不到发布源的服务端。

两者流程相同：解出新二进制并试运行 `version`（架构不对、跑不起来、本地构建都在改动前拒绝），再问它是否支持控制台升级
（`gpuworks console-upgrade-protocol`；控制台升级之前的版本不认识它，不会确认或回滚自己的启动，因此拒绝并提示在主机上用
`gpuworks upgrade --archive` 安装）→ 用 `VACUUM INTO` 拍下运行中的数据库，
连同 `master.key`、`server.yaml` 与当前二进制打成和 CLI 相同格式的备份 `<dir>/backups/<release_id>-<时间>.tar.gz` →
写升级标记 `<dir>/tmp/upgrade-pending.json` → 原子替换二进制 → 退出，由 systemd 或守护进程拉起新版本。新版本启动时先看标记：
在 15 分钟内健康地以新版本应答就确认升级（写 `install-history.jsonl`、删标记、只保留最近 3 份备份）；连续 3 次启动都没起来，
或起来了却一直不健康，下一次启动就还原备份（数据库、主密钥、`server.yaml` 与旧二进制）并换回旧版本。页面会等新版本应答后刷新。
标记写明目标版本：启动的不是该版本（二进制已被 `gpuworks upgrade` 或手工换掉）时，标记作废并删除，不计次、不回滚。

服务端不满足上面的条件时，页面说明原因并给出命令：装好的服务端只是没有被托管（例如手工执行 `serve --config` 跑起来的），页面给出
`<dir>/gpuworks restart --dir <dir>`（改由守护进程运行，之后就能在页面升级）和容器里应改用的启动命令
`<dir>/gpuworks serve --supervise --config <dir>/server.yaml`；其他情况（没有安装、运行的不是安装目录里的二进制、安装目录不可写）给出 `sudo gpuworks upgrade`。

### 离线升级

主机访问不到任何发布源时，把新版发布包拷到主机上，用 `--archive` 升级，流程（停服、备份、替换、探活、失败回滚）与在线升级完全相同，不发任何网络请求：

```bash
sha256sum -c SHA256SUMS --ignore-missing      # 和发布包一起拷来的校验文件
/opt/gpuworks/server/gpuworks upgrade --archive ./gpuworks-<release_id>-linux-amd64.tar.gz --check
/opt/gpuworks/server/gpuworks upgrade --archive ./gpuworks-<release_id>-linux-amd64.tar.gz
```

- 不用先解压；目标版本取自包里二进制的 `gpuworks version`，`--version ID` 可以再加一道核对。
- 包里的二进制跑不起来（架构不对、包损坏）或是本地构建（`dev` / `-dirty`）时，在停服前报错退出，什么都不改。
- 不要用新包里的 `./gpuworks install` 代替升级：重复 install 虽然也换二进制、不动数据库，但不做备份、也不会回滚。
- 节点 agent 照常在节点页「升级 Agent」：新 agent 内嵌在 server 二进制里，同样不需要访问发布源。

## 卸载

运行卸载命令后，按提示选择是否清理数据、检查清单，再确认执行：

```bash
sudo gpuworks uninstall --dir /opt/gpuworks/server
```

systemd、nohup、macOS launchd 使用同一命令。卸载会先确认进程退出，再移除所属服务定义、取消自启并清理程序；同安装的 expose 公网入口也会关闭。普通卸载保留配置、数据库、`master.key`、日志与备份，支持重装恢复。

自动化通过显式参数选择范围，不等待输入：

```bash
sudo gpuworks uninstall --dir /opt/gpuworks/server --yes
sudo gpuworks uninstall --dir /opt/gpuworks/server --purge --yes --json
```

`--purge` 会删除本安装已知配置、数据、备份和日志，包含配置指定的外置目录；执行前可用 `--purge --dry-run --json` 预览清单。未知文件、服务账号和组保留；互斥锁 `.upgrade.lock` 及其安装目录也保留，避免并发升级/卸载绕开同一把锁。服务账号 home 可能就是安装目录，也可能属于已有账号，不会使用 `userdel -r` 删除。

软链接、挂载点、用户 home 和覆盖安装目录的数据路径会拒绝自动清理；非空数据目录需有可识别的 GPUWorks 数据库或 master key。遇到这些情况先检查实际路径和归属，再单独处置相应资源。

非交互运行必须传 `--yes`（预览除外）；`--quiet` 只隐藏说明，不跳过确认。返回码 `0` 表示完成，`1` 表示失败，可修复原因后再次执行。参数与 JSON 字段见 [CLI 参考](../reference/cli.md#gpuworks-uninstall)。

远端 agent、推理服务、云租机以及 Cloudflare tunnel / DNS 需单独处置，卸载控制面不会回收它们，云资源可能继续计费。

## 排查

| 现象 | 处理 |
|---|---|
| 安装失败在探活一步 | 看 `<dir>/logs/gpuworks-server.log`（轮转规则见[日志与轮转](../reference/install-layout.md#日志与轮转)）；失败进程已被停掉，可反复重试 |
| `cannot listen on :8800` | 端口被别的进程占用；确认不是旧安装的残留进程（`systemctl status gpuworks`） |
| `create /opt/gpuworks/server needs root` | 用 `sudo`，或改 `--dir ~/gpuworks-server --no-systemd` |
| 忘记密码 | `gpuworks reset-password --password NEW --data <data目录>`（操作员已在 `gpuworks` 组时不需要 sudo） |
| 升级或 install 报权限错误 | 当前终端还没拿到 `gpuworks` 组：`newgrp gpuworks` 或重新登录 |
| 升级后节点全部离线 | 多为节点接入监听地址改了但转发（反代/NAT）没跟着改；检查设置页「监听地址」与节点版本，见[配置节点网络](configure-networks.md#监听地址与别人怎么连过来) |
