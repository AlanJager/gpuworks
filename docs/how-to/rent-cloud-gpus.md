# 租用云 GPU

**适用场景**：没有现成的 GPU 主机，想用云厂商（阿里云、RunPod、优云智算 CompShare、AutoDL、Vast.ai）上的 GPU 实例，并一键安装 agent 把它接入 GPUWORKS。

**前置条件**：

- server 已运行（见[安装和升级控制面](install-server.md)），你能登录控制台。
- 有对应云厂商的账号和 API 凭据（凭据字段见下表），账户里有可用余额，并已在云厂商一侧有一台 GPU 实例（或打算用下文的 API 创建）。
- 云实例要能访问你填写的 `server_url`。server 在 NAT 后面时，请提供稳定的域名或反代地址，不要用 `gpuworks expose` 的 quick 隧道地址（见[给控制台一个公网地址](expose-public-address.md)）。

GPUWORKS 对云实例做的事是：保存云账号凭据（加密存储）→ 同步实例清单 → 经 SSH（阿里云优先经云助手）在实例上安装
agent → 实例作为“计算节点”上线。**控制台里的“公有云”页不提供“选机型、选区域、下单”的界面**：
租机（选机型、区域、镜像）请先在云厂商自己的控制台完成，再回到 GPUWORKS 同步并接入。通过 REST API
创建/删除实例的方式见[用 API 创建和释放实例](#用-api-创建和释放实例)。

## 添加云账号

1. 左侧菜单“资源”分组下打开“公有云”页（路径 `/providers`）。
2. 点“添加公有云”，或点页面中部“快速接入”里的某个厂商卡片（会预选该类型）。卡片下方是厂商官网链接。
3. 选择类型，填写“名称”（自定义，必填）和“认证信息”里的字段，点“添加”。

各类型的凭据字段（以界面显示为准，带“可选”的可以不填）：

| 类型 | 必填 | 可选 |
|---|---|---|
| 阿里云 | AccessKey ID、AccessKey Secret | RAM 角色 ARN、地域（见[阿里云](#阿里云)） |
| RunPod | API Key | API 接口地址 |
| 优云智算（CompShare） | 公钥、私钥（优云智算 API 密钥对） | API 接口地址 |
| AutoDL | API Key | API 接口地址、SSH 密码（之后装 agent 时作为默认 SSH 密码） |
| Vast.ai | API Key | API 接口地址 |

AutoDL 的 API 只列出「容器实例Pro」，其他类型的实例不会出现在实例表里（添加账号的表单里也有这条提示）。AutoDL Pro 实例只开放 HTTP 端口，所以只能用“Agent 发起连接”。

RunPod 只支持 Secure Cloud 的 Pod，不支持 Community Cloud（添加账号的表单里也有这条提示）。

“API 接口地址”留空时使用各厂商默认地址，只有走代理或私有网关时才需要改。

保存时 server 会用这些凭据调用云厂商接口做一次校验：通过才保存，状态显示“有效”；不通过则提示
“添加失败：…”，并带上云厂商返回的原因。保存后凭据加密存储，界面不再回显。之后可在列表里点“编辑”
更新凭据：**没填的字段保留原值**，类型不能改。

ZStack Cloud 不在“公有云”页，而在“专有云”页（路径 `/zstack`）：需要“API 接口地址”和“管理员 Token”，
当前只做接入和凭据校验，不能同步实例，也不能安装 agent（该页上显示“即将支持”）。

## 同步实例并确认机型、区域和价格

1. 在“已接入公有云”列表里点某个账号的“同步”（或页面上方“同步全部”一次同步所有账号）。完成后提示“同步完成，发现 N 个实例”。
2. 点开该账号所在行左侧的展开箭头，看实例表：

   | 列 | 含义 |
   |---|---|
   | 实例名称 / 实例 ID | 云厂商侧的名称与 ID |
   | GPU 型号 / GPU 卡数 | 云厂商返回的规格；拿不到时显示“未返回” |
   | 连接方式 | 云厂商提供的入口：“直连”或“平台代理”（SSH，可复制 `ssh` 命令；标注“仅终端”的入口只能连接，不能用来装 agent），阿里云还有“云助手”（无需 SSH） |
   | 状态 | 运行中 / 已停止 / 未知 |
   | 费用 | 每小时价格（如 `¥1.98/时`）；云厂商没返回价格时可能显示 `¥0.00/时`，不代表免费 |
   | 区域、类型 | 区域；类型为“容器”或“虚拟机” |

3. 账号状态会随同步结果变化：同步失败会把账号标为“异常”，页面上方出现“存在异常公有云”提示，按提示检查凭据和云厂商 API 可用性，再重新同步。

列表每 15 秒自动刷新一次账号状态，但实例清单来自上一次“同步”的结果，想看云端最新情况请重新点“同步”。“容器”类型的实例没有 root
和 Docker 守护进程，agent 会自动使用内置的 runveil 运行时。

## 安装 agent，让实例出现在“计算节点”

1. 在实例表里，找到还没接入的实例，点“一键装 Agent”。已接入的实例显示绿色“已接入”，不会再出现此按钮。
2. 在“安装 agent”弹窗里填写：
   - **SSH 凭据默认不用填**：server 用自己的平台密钥登录（见[接入节点](connect-nodes.md)），并按云厂商自动让实例接受它——RunPod 把平台公钥追加到账号 SSH Key（保留你已有的 key；添加 RunPod 账号时、在 server 上新建 Pod 前都会注册；RunPod 只在 Pod 启动时写入，所以对之后启动的 Pod 生效），Vast 经接口挂到实例，CompShare 与 AutoDL 用接口返回的实例 root 密码登录一次再写入平台公钥（AutoDL 凭据里保存的 SSH 密码作为后备）。之后带外恢复只用平台密钥，不保存密码。已在运行、又没拿到平台公钥的 RunPod Pod（用已有凭据登录不了时），接口返回 409 `INSTANCE_RESTART_REQUIRED`（`need` 为 `ssh_key`，同时缺 TCP 端口时为 `ssh_key_port`）、什么也没改，弹窗问“RunPod 只能通过修改 Pod 配置添加公钥，这会重启容器并清空非持久卷”；确认后驱动把平台公钥追加到 Pod 的 `PUBLIC_KEY` 环境变量（需要时同一次修改里加上端口），等 Pod 重启、平台密钥能登录后再安装，检查表里是 `Platform key (pod)` 一行。这种 Pod 的预检查里 `SSH` 一行是警告而不是失败，说明安装时会添加公钥（其余检查要登录后才能跑）。也可以不重启，在「高级设置」里提供一次私钥，登录后同样写入平台公钥。检查表开头的「Platform key (provider)」「Instance password」两行说明 server 为登录做了什么。
   - **连接方式**：默认“自动选择”：阿里云先用云助手，其余先用直连；失败会自动回退到下一个入口。也可以手动指定某个入口。
   - **接入方式**：默认“Agent 发起连接”，实例主动连接 server，不需要实例开放入站端口。“Server 发起连接”由 server 拨入实例，此时弹窗不再要求管理节点接入地址（agent 不连接 server），改为要填「Agent 监听地址」（默认 `0.0.0.0:18800`，agent 在实例里监听的地址）和「计算节点接入地址」（server 连这台实例用的地址；只填主机时端口与监听端口相同，平台把端口映射成别的号码时填映射后的 `host:port`）。该方式要求 server 能访问到那个 TCP 端口，实例的安全组或防火墙要放行；一般不建议。各厂商的差别：
     - **阿里云**：云助手只能让实例自己从 server 下载 agent，而需要 server 拨入的实例通常下载不到，所以“自动选择”时 `server_dial` 直接走 SSH（上传 agent，实例不必能访问 server），不会先试云助手。预检查经 API 只读实例的安全组（不做任何修改），多一行 `Security group`：没有入方向规则放行 server 要拨的 TCP 端口时报失败，并说明去哪个安全组加规则；只对部分来源放行时报警告，因为 server 的出口地址要落在其中。若安装时 server 还连不上，结果会显示“agent 已启动，server 还连不上它”，放行端口后 server 会自动连上，不用重装。
     - **AutoDL**：不提供“Server 发起连接”（Pro 实例只开放 HTTP 端口）；要用的话按 AutoDL 的文档开放 TCP 端口，再在“添加计算节点”里手工接入。
     - **Vast.ai、RunPod**：容器只能经平台映射的 TCP 端口到达，端口号和公网地址由平台决定。两个地址栏都留空时，驱动经 SSH 读 `/proc/net/tcp`，在实例已映射且没有进程监听的 TCP 端口里选一个（SSH 的端口不用），把监听端口和映射后的公网地址记到节点上，检查表里多一行 `Server-dial port`。端口要到安装时才选，所以预检查不拨实例的默认端口，`Server to agent` 一行写明在安装时检查。Vast 的端口在创建实例时就固定了（`vastai` 基础镜像会多映射 `10100`、`10200` 之类的备用端口），实例没有空闲的映射端口就只能用“Agent 发起连接”，或换一个映射了备用端口的模板重租。RunPod 优先用 Pod 已开放的空闲 TCP 端口；一个都没有时接口返回 409 `INSTANCE_RESTART_REQUIRED`、什么也没改，弹窗会问“RunPod 只能通过修改 Pod 配置开放端口，这会重启容器并清空非持久卷。确认自动添加端口？”，确认后（请求带 `allow_instance_restart`）驱动为 Pod 添加一个 TCP 端口，这会重启 Pod 并把所有端口（包括 SSH）重新编号，驱动等到新映射和 SSH 都就绪（最长 5 分钟）再继续装 agent。
   - **安装目录与「通过 systemd 启动」**（「高级」里，含义同[接入节点](connect-nodes.md)的 SSH 向导）：安装目录留空为 `/opt/gpuworks`，填的话必须是 `/` 开头的绝对路径，例如数据盘 `/data/gpuworks`。「通过 systemd 启动」默认开启，有 systemd 且能用 root 的实例注册 `gpuworks-agent.service`；关闭后 agent 以 nohup 运行、开机不自启，主机上已有的服务和其他目录里的 agent 都不动。容器实例（RunPod、Vast、AutoDL）通常没有 systemd，开不开都走 nohup。
   - **管理节点接入地址**（`server_url`）：弹窗用浏览器当前访问的地址预填。**它必须是云实例能访问到的稳定地址**，在 NAT 后面时改成域名或反代地址。填了 `*.trycloudflare.com` 会出现“这看起来是 Cloudflare 快速隧道地址”的警告：这种地址在连接器重建后就变，节点之后会连不上。格式必须是不带用户信息、查询和片段的 `http://` 或 `https://` 绝对地址，否则接口返回 `SERVER_URL_REQUIRED` / `SERVER_URL_INVALID`。
3. 点“安装”。server 先做 SSH 预检查，再经 SSH 在实例上安装并启动 agent，最后检查 agent 是否已建立会话。弹窗下方的检查表列出 `precheck`、`install agent`、`agent_dial` 等项的“通过 / 警告 / 失败”。没有警告和失败时弹窗自动关闭；有警告时保留在弹窗里让你看详情（例如 agent 已启动但还在等第一次连接，稍等片刻再看“计算节点”页即可）。
4. 打开“计算节点”页：以实例名命名的节点出现并在线后，就可以按[接入、升级和卸载节点](connect-nodes.md)里的流程做后续事情（探测 GPU、部署模型等）。安装失败时，这次创建的待接入节点会被清掉，不留半成品，修好原因后重新点安装即可。

云一键安装只是[接入节点](connect-nodes.md)的三种方式之一；同样的实例也可以用 SSH 向导或 `/install.sh` 手工装。

租来的节点自带系统标签 `gpuworks.ai/provider`（云厂商）和 `gpuworks.ai/region`（区域），可以用它们建[同步组](configure-pull-settings.md#同步组)：不同厂商或区域的云主机通常互不相通，不该互相复制模型和镜像。

## 停止和释放实例，避免继续计费

在 GPUWORKS 里能做的：

- **关机**：在实例行点“关机”并确认。只有支持电源控制的类型才有这个按钮（优云智算、AutoDL）；“开机”同理。点了之后提示“已发送关机请求”，状态以随后的同步结果为准。
- **释放（销毁）**：界面上没有释放实例的按钮。RunPod、Vast.ai 不支持电源控制，也就没有关机按钮，要停止计费只能在云厂商控制台销毁实例，或用 REST API 释放。
- **删除云账号**（列表里的“删除”）只会删除 GPUWORKS 里的账号、凭据和实例记录，**不会**关闭或销毁云上的实例，也不会删除已接入的节点。
- 用 API 释放成功后，该实例立即从实例列表里消失；释放失败时列表不变。在云厂商控制台销毁或到期回收的实例，下一次同步成功时从列表里移除；同步失败（账号标为“异常”）不改动列表。
- 实例上已接入的节点不会随之删除：节点保留，带外恢复阶段直接标为 `stopped`（原因 `provider instance no longer exists`），不再尝试 SSH 恢复，见[排障](troubleshooting.md)；确认不再需要后手动删除节点。

计费规则（关机后是否继续收取磁盘、带宽等费用）由云厂商决定，以各家控制台的账单为准。**用完的实例请到云厂商控制台确认已销毁，并定期核对账单**。

## 用 API 创建和释放实例

如果想不进云厂商控制台，直接由 GPUWORKS 下单/释放，用 REST API（需要 JWT，字段见 [API 参考](../reference/api.md)）：

- `POST /api/v1/providers/{id}/instances`：创建，请求体为 `name`、`region`、`gpu_model`、`gpu_count`、`image` 和字符串键值的 `extra`，成功返回 201 和实例信息。
- `DELETE /api/v1/providers/{id}/instances/{externalID}`：释放。
- `POST /api/v1/providers/{id}/instances/{externalID}/power`：请求体 `{"action":"on"}` 或 `{"action":"off"}`。

创建之后，到“公有云”页点“同步”，实例才会出现在列表里。各类型的要求不同：

| 类型 | 创建时的要求 |
|---|---|
| RunPod | `gpu_model` 必填（RunPod 的 GPU 型号 ID）；`gpu_count` 默认 1；`image` 默认 `runpod/pytorch:...`；`extra` 可带 `disk_gb`（默认 20）、`volume_gb`、`ports` 等 |
| 优云智算 | `gpu_model` 和 `image`（优云智算镜像 ID）必填；`gpu_count` 默认 1；`region` 缺省用厂商默认区域；`extra` 可带 `zone`、`disk_gb`（默认 100）、`cpu`、`memory_mb`、`password` 等 |
| AutoDL | `image`（镜像 UUID）必填；GPU 型号需能映射到已知规格，或在 `extra.gpu_spec_uuid` 里给出；`gpu_count` 1–4；`region` 为数据中心列表 |
| Vast.ai | `extra.offer_id` 必填（先在 Vast.ai 上搜索并选定一个报价，价格由该报价决定）；`image` 有默认值；`extra.disk_gb` 默认 20 |

这些要求来自各 provider 的实现，以接口返回的错误信息为准；创建前先确认余额和库存。释放：RunPod、Vast.ai 直接销毁；优云智算、AutoDL 会先关机再释放。`resize` 接口目前四家都返回 501（不支持）。

## 常见问题

**添加账号时报“添加失败：credential validation failed …”**
凭据没通过云厂商校验：检查 API Key / 公钥私钥是否复制完整（前后有空格或换行）、是否属于对应平台、账号是否被限制，以及 server 主机能否访问云厂商 API（“API 接口地址”是否需要走代理）。编辑已有账号时，没填的字段保留旧值，换了 Key 就要把新 Key 填进去。

**列表里账号显示“异常”，或点“同步”报“同步失败”**
同步会真实调用云厂商接口，失败即标“异常”。先按失败信息排查凭据是否被吊销、账户是否欠费，再点“同步”；成功后状态自动回到“有效”。

**实例表是空的（“未同步到实例，请点击上方‘同步’后重试”）**
还没同步，或云厂商账号下确实没有实例。先点“同步”。

**点“一键装 Agent”失败**
安装结束后弹窗顶部会有一条结论：绿色“agent 已安装并已连接”、黄色“已安装，有需要留意的项”或“已启动，尚未连上”、红色“安装失败”（后面跟原因）。装好后只剩“完成”按钮，关闭弹窗时实例列表会刷新；失败时表单保留，“重试”前可以改连接方式或在「高级」里填凭据。再往下看弹窗里的检查表和告警：
- `precheck` 一行只列出没通过的项（全部通过时写“all N checks passed”），完整清单在“预检查明细”里展开。`precheck` 项失败：SSH 入口不可达、用户名/密码/私钥不对，或选了“仅终端”的入口。试试换一个“SSH 连接方式”；自动选择会在直连失败时回退，失败详情会以告警列出每个入口的原因。
- `precheck` 或安装详情里的 `Install directory …` 项：云一键安装默认装到 `/opt/gpuworks`，可在安装弹窗「高级」里改（必须是 `/` 开头的绝对路径，不展开 `~`），预检查和安装都会检查它背后的文件系统（六个检查项的含义见[接入节点](connect-nodes.md#安装目录检查)）。容器实例（例如 RunPod）上默认目录在容器可写层，实例重建就丢；`Install directory persistence` 会**警告**：这是 agent 在云容器实例上唯一能放的位置，实例删除或重建后要重装；`/workspace` 这类持久卷往往是不支持改属主的网络文件系统，会出现 `ownership` 和 `filesystem` 警告；RunPod 的 `/workspace` 还不允许文件名带冒号，`file names` 项**失败**，udocker 运行时在那里跑不了部署，agent 要装在容器盘上（并在租机时把容器盘开到放得下镜像）。需要把 agent 装进持久卷或数据盘时，在「高级」里填安装目录；填了的目录写不进时安装失败，不会回退到 `/tmp/gpuworks`。`noexec` 的目录会让安装直接失败。
- `SSH authentication` 失败且只试了平台密钥：实例还没有平台公钥（例如 RunPod 上 server 注册公钥前就已启动的 Pod：从“公有云”页安装时会询问是否重启 Pod 加上公钥），在「高级设置」里提供一次私钥或密码，或按错误里给出的命令在实例上授权。
- 云厂商没有返回 SSH 入口（列显示“未返回”）：实例可能还在启动，过一会儿再点“同步”。
- `agent_dial` 项警告：agent 已启动但还没连上 server，多半是 `server_url` 对实例不可达，见下一条。

**安装成功，但“计算节点”页没有出现节点，或一直离线**
最常见的原因是 `server_url` 对云实例不可达（填了内网地址、`localhost`，或 quick 隧道的临时域名）。到实例上看 agent 日志确认连接报错，把 `server_url` 改成稳定的域名或反代地址后重新安装。接入后的被拒、掉线排查见[排障](troubleshooting.md)。

**误留了实例，产生了费用**
GPUWORKS 不会自动释放云实例，删除云账号也不会。到云厂商控制台销毁不用的实例，必要时用上文的 API 释放；销毁后在 GPUWORKS 里到“计算节点”页删除对应的离线节点。

## 相关文档

- [接入、升级和卸载节点](connect-nodes.md)
- [给控制台一个公网地址](expose-public-address.md)
- [排障](troubleshooting.md)
- [测试指南（云端 L4 用例与清理）](../dev/testing.md)
## 阿里云

**凭据**：填 AccessKey ID 和 AccessKey Secret，主账号或 RAM 子账号的 AccessKey 都可以。主账号 AccessKey 拥有账号的全部权限，**建议新建一个只有下面权限的 RAM 子账号**，用它的 AccessKey。

“地域”留空时同步账号所有地域（几十个，并发查询，几秒完成）；只在部分地域有实例时填逗号分隔的地域 ID（如 `cn-shanghai,cn-beijing`）可以减少调用。

**需要的权限**（RAM 自定义权限策略。2026-10-15 在真实账号上验证过：只有这些权限的子账号、以及只能扮演角色的子账号加上带这些权限的角色，都能完成同步、安装前检查和开关机，未授予的操作如重启被拒绝）：

| 权限 | 用途 |
|---|---|
| `ecs:DescribeRegions` | 校验凭据、枚举地域 |
| `ecs:DescribeInstances` | 同步实例；开关机前定位实例所在地域 |
| `ecs:DescribeCloudAssistantStatus` | 判断实例的云助手是否在线 |
| `ecs:RunCommand`、`ecs:DescribeInvocationResults` | 经云助手执行安装前检查和安装，并读取结果 |
| `ecs:StartInstance`、`ecs:StopInstance` | 开机、关机；不需要可以去掉 |

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ecs:DescribeRegions",
        "ecs:DescribeInstances",
        "ecs:DescribeCloudAssistantStatus",
        "ecs:RunCommand",
        "ecs:DescribeInvocationResults",
        "ecs:StartInstance",
        "ecs:StopInstance"
      ],
      "Resource": "*"
    }
  ]
}
```

GPUWORKS 不在阿里云上创建或释放实例，不需要这两类权限。

**在控制台创建子账号**：

1. 打开 [RAM 访问控制](https://ram.console.aliyun.com/)，「权限管理 → 权限策略 → 创建权限策略」，切到「脚本编辑」，粘贴上面的 JSON，命名为 `gpuworks-ecs` 保存。
2. 「身份管理 → 用户 → 创建用户」，填登录名（如 `gpuworks`），访问方式勾选「使用永久 AccessKey 访问」，确定后**立即保存** AccessKey ID 和 Secret（Secret 只显示这一次）。
3. 在用户列表里给这个用户「添加权限」，选「自定义策略」里的 `gpuworks-ecs`。
4. 回到 GPUWORKS，用这对 AccessKey 添加阿里云账号。

**用命令行创建**（[阿里云 CLI](https://help.aliyun.com/zh/cli/)，用有 RAM 管理权限的身份执行；也可以把这段交给 agent 执行）：

```bash
POLICY='{"Version":"1","Statement":[{"Effect":"Allow","Action":["ecs:DescribeRegions","ecs:DescribeInstances","ecs:DescribeCloudAssistantStatus","ecs:RunCommand","ecs:DescribeInvocationResults","ecs:StartInstance","ecs:StopInstance"],"Resource":"*"}]}'
aliyun ram CreatePolicy --PolicyName gpuworks-ecs --PolicyDocument "$POLICY"
aliyun ram CreateUser --UserName gpuworks
aliyun ram AttachPolicyToUser --PolicyType Custom --PolicyName gpuworks-ecs --UserName gpuworks
aliyun ram CreateAccessKey --UserName gpuworks   # 输出里的 AccessKeyId / AccessKeySecret 就是要填的凭据
```

**可选：RAM 角色**。企业把权限收在角色里、或跨账号授权时，权限策略授给角色而不是用户，用户只保留扮演角色的权限，再在“RAM 角色 ARN”里填角色 ARN（`acs:ram::<账号 ID>:role/<角色名>`）。server 用用户的 AccessKey 调 STS `AssumeRole` 换取一小时的临时凭据，之后的调用都用临时凭据签名，快到期时自动续：

```bash
ACCOUNT=$(aliyun sts GetCallerIdentity | python3 -c 'import sys,json;print(json.load(sys.stdin)["AccountId"])')
TRUST="{\"Statement\":[{\"Action\":\"sts:AssumeRole\",\"Effect\":\"Allow\",\"Principal\":{\"RAM\":[\"acs:ram::$ACCOUNT:root\"]}}],\"Version\":\"1\"}"
aliyun ram CreateRole --RoleName gpuworks --AssumeRolePolicyDocument "$TRUST"
aliyun ram AttachPolicyToRole --PolicyType Custom --PolicyName gpuworks-ecs --RoleName gpuworks
aliyun ram AttachPolicyToUser --PolicyType System --PolicyName AliyunSTSAssumeRoleAccess --UserName gpuworks
```

这时用户本身不需要 `gpuworks-ecs`（可以 `DetachPolicyFromUser` 去掉），ECS 权限全部来自角色。

**同步**：实例表只列出账号在各地域带 GPU 的 ECS 实例（不带 GPU 的实例不显示），GPU 型号与卡数来自实例规格。运行中的实例同时查询云助手是否在线：在线的实例连接方式里出现“云助手”；有公网 IP（含 EIP）的实例还有“直连”（`root@<公网 IP>:22`）。Windows 实例不能安装 agent。

**安装**：“自动选择”时先走云助手（agent 由 server 拨入的 `server_dial` 除外，见上）——server 调 `RunCommand`，让实例以 root 执行与“添加节点”相同的一行安装命令（`curl <server_url>/install.sh | sh -s -- …`），实例自己从 server 下载 agent。这条路不需要 SSH、密码、平台密钥，也不需要公网 IP，但**实例必须能访问 `server_url`**：没有公网出口的实例只能用同一 VPC（或已打通的网络）里能访问到的 server 地址，见[节点与 server 网络不通](connect-nodes.md#节点与-server-网络不通)。安装前的检查同样经云助手执行，并从 server 一侧检查能否反向连到实例。

经 SSH 安装（云助手不在线，或 `server_dial`）之前，只要云助手在线，server 先经云助手把平台公钥加进实例 root 的 `~/.ssh/authorized_keys`（已有就不重复加，检查表里是 `Platform key (Cloud Assistant)` 一行），再用平台密钥登录，不需要提供任何凭据。还没有平台公钥的实例，预检查里 `SSH` 一行是警告，说明安装时会经云助手添加（其余检查要登录后才能跑）。云助手不在线时，安装最多等 2 分钟让它上线（刚开机的实例 SSH 先可连，云助手约一分钟后才上报第一次心跳），等到了再加公钥，检查表里写明等了多久。等不到（自定义镜像没有预装云助手）时，阿里云实例没有平台密钥，也不会经接口给出 root 密码，需要在「高级设置」里提供一次密码或私钥。要用云助手，可以在阿里云控制台为实例安装云助手（需要重启实例）。

云助手的调用记录（含执行的命令）会在阿里云账号里保留 30 天。命令里带着该节点的接入 token，agent 之后一直用它连接 server，所以有权限查看云助手调用记录（`ecs:DescribeInvocations`）的阿里云用户能读到它；节点删除后 token 随之失效。

**开关机**：关机使用“普通停机”（继续计费），保留实例的 GPU、固定公网 IP 等资源；“节省停机模式”会释放这些资源，再开机时可能没有库存，GPUWORKS 不使用它。

## ZStack Cloud 连接凭据

添加专有云时填写 API Endpoint、管理员的 AccessKey ID 和 AccessKey Secret。使用 ZStack 的
本地 AccessKey，无需保存或刷新登录 Session。AccessKey 必须处于启用状态，所属账号须有查询
物理机、GPU 和资源配置以及执行 GPU 资源联动的权限；ZStack 的 AccessKey 认证要求有效企业版许可证。
Endpoint 支持包含 `/zstack` 上下文和可选的 `/v1` 后缀。

API 沿用公共凭据字段：`public_key` 保存 AccessKey ID，`private_key` 保存 Secret；两者随整个
凭据对象加密存储。界面根据服务端字段字典显示 AccessKey 名称，Secret 使用密码输入框。
已经配置 Session Token 的专有云，需要通过编辑凭据填写这两个字段；原 Session 无法转换为 AccessKey。

请求按 [ZStack AccessKey 协议](https://www.zstack.io/help/dev_manual/dev_guide/v4/3.html) 签名。
GPUWorks 与 Cloud 的系统时间需要同步。旧版 Cloud 配置 `rest.checkTimeZone=false` 时会忽略
时区偏移，两端系统时区也须一致；签名保留 GPUWorks 进程的本地时区，与 ZStack 官方 SDK 一致。
查询和资源变更请求都使用 AccessKey，Secret 不随请求发送。
