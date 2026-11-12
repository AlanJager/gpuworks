# 在 Windows WSL2 上用 Docker 运行 GPUWorks

本文说明如何在 Windows 的 WSL2（Ubuntu）里安装控制面与本机 GPU 节点，并用 **Docker + NVIDIA Container Toolkit** 作为推理运行时，完成小模型部署与调用。

**适用场景**：只有 Windows 工作站或笔记本、带 NVIDIA GPU，希望本机试用 GPUWorks，浏览器访问 `http://localhost:8800`。

**前置条件**：

- Windows 已安装最新 NVIDIA 驱动；**不要**在 WSL 内再装 Linux 版 NVIDIA 驱动（WSL 使用 Windows 驱动透传）。
- 已启用 WSL2，并安装 Ubuntu（建议 22.04/24.04）。
- 发行版已开启 systemd（`/etc/wsl.conf` 含 `[boot] systemd=true`，改完后执行 `wsl --shutdown` 再进入）。
- 宿主机系统盘建议至少空出约 **50 GiB**（小模型冒烟时，runner 镜像在 Docker 中展开后可达约 30 GiB 量级，另加模型权重与 WSL 虚拟磁盘增长）。
- 验收用小模型（例如 Qwen3-1.7B 量级）。16 GiB 消费级显存不适合用数十 GiB 权重的大模型做本机验收。

GPUWorks 没有单独的 Windows 安装包：控制面和 agent 都按 Linux 方式装在 WSL 发行版里。与普通 Linux 主机相比，WSL 上要额外处理两件事：推理运行时必须是 Docker，以及让发行版常驻。

## 为什么必须用 Docker

WSL2 上 agent 在未检测到可用的 Docker 时，会改用 runveil/udocker。在该路径下启动 vLLM 类 runner 时，容易因镜像内 CUDA compat 的 `libcuda` 与 WSL 的 `/dev/dxg` 路径不兼容而失败，日志常见：

```text
RuntimeError: Failed to infer device type
```

因此本指南要求：

1. 安装 Docker Engine 与 NVIDIA Container Toolkit。
2. 确认 agent 日志为 `runtime driver detected driver=docker` 后再部署。

runveil/udocker 的一般说明见[runveil 与 udocker](../explanation/runveil-and-udocker.md)；本页只覆盖 WSL2 上的可行路径。

## 1. 配置 WSL 常驻（避免浏览器突然连不上）

编辑 Windows 用户目录 `%USERPROFILE%\.wslconfig`：

```ini
[wsl2]
memory=18GB
swap=2GB
processors=8
localhostForwarding=true
vmIdleTimeout=-1

[general]
instanceIdleTimeout=-1
```

说明：

- `vmIdleTimeout=-1` 只避免空闲时关掉 WSL **虚拟机**。
- `instanceIdleTimeout=-1` 才避免 Ubuntu **发行版实例**空闲退出；缺少这一项时，`wsl -l -v` 仍会变成 `Stopped`，`localhost:8800` 不可达。
- 内存与处理器数值按本机资源调整；改完后执行 `wsl --shutdown`，再重新进入发行版。

可选：用计划任务在登录时及周期性执行 `wsl.exe -d <发行版名> --exec /bin/true`，作为兜底唤醒。

访问控制台前先确认：

```powershell
wsl -l -v
```

目标发行版应为 `Running`。

## 2. 在 WSL 中安装 Docker 与 NVIDIA Container Toolkit

在 WSL 终端安装 Docker Engine（按 Docker 官方 Ubuntu 文档），然后：

```bash
sudo systemctl enable --now docker
docker version
```

安装 NVIDIA Container Toolkit（按 NVIDIA 官方文档），并配置 Docker：

```bash
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

验证 GPU 进入容器：

```bash
docker run --rm --gpus all --entrypoint nvidia-smi ubuntu:24.04 -L
```

应能列出本机 GPU。若 Docker Hub 拉取超时，先配置可用的镜像加速或换可访问的基础镜像，再继续。

runner 镜像有数十 GiB，同时下载的层越多，WSL 的内存与磁盘压力越大。建议限制 Docker 的并发下载数。`nvidia-ctk` 已把 NVIDIA 运行时写进 `/etc/docker/daemon.json`，所以下面合并写入这一项，不覆盖原文件：

```bash
sudo python3 - <<'EOF'
import json, pathlib
path = pathlib.Path("/etc/docker/daemon.json")
config = json.loads(path.read_text()) if path.exists() else {}
config["max-concurrent-downloads"] = 2
path.write_text(json.dumps(config, indent=2) + "\n")
EOF
sudo systemctl restart docker
```

## 3. 安装控制面

在同一 WSL 发行版中：

```bash
bash <(curl -fsSL https://dl.gpuworks.ai/get.sh) --quiet --addr 0.0.0.0:8800
```

- `--addr 0.0.0.0:8800`：默认监听地址是 WSL 虚拟网卡的地址，Windows 经 `localhost` 转发访问不到，所以这里改为监听所有地址，agent 也经 `127.0.0.1` 连接。WSL 使用 mirrored 网络模式（`.wslconfig` 中 `networkingMode=mirrored`）时，这个地址会把控制台暴露到局域网，务必先修改 admin 密码，并用 Windows 防火墙限制 8800 端口的入站访问。
- 需要交互确认时去掉 `--quiet`。更多选项见[安装和升级控制面](install-server.md)。

验证：

```bash
systemctl is-active gpuworks
curl -fsS http://127.0.0.1:8800/healthz
```

在 Windows 浏览器打开 **http://localhost:8800**。使用安装输出中的 `admin` 密码登录；密码也写在数据目录的 `initial-credentials`（见[控制面安装布局](../reference/install-layout.md)）。登录后请改密并删除该文件。

## 4. 接入本机节点

1. 打开 **计算节点 → 添加节点**。
2. 连接方式选 Agent 主动连接（`agent_dial`），Server URL 填 `http://127.0.0.1:8800`。
3. 在同一 WSL 终端执行页面生成的安装命令（形如 `curl …/install.sh | sh -s -- --server … --token …`）。细节见[接入节点](connect-nodes.md)。

确认 agent 使用 Docker：

```bash
journalctl -u gpuworks-agent -n 50 --no-pager | grep 'runtime driver'
```

期望输出包含：

```text
runtime driver detected driver=docker
```

Docker 没被选中时，同一条日志的 `driver` 会写明原因：

- `udocker (no docker daemon)`：没有找到 Docker。确认 `docker.service` 为 `active`、`/var/run/docker.sock` 存在，然后执行 `sudo systemctl restart gpuworks-agent`。
- `udocker (docker daemon not answering)`：Docker 已安装但没有响应，agent 每分钟复查一次，Docker 恢复且节点上没有实例在运行时自动切回。
- `udocker (docker cannot use the accelerators: …)`：Docker 不能把 GPU 交给容器，前面还有一行 `runtime driver passed over: it cannot give containers this node's accelerators`。按第 2 节配置 NVIDIA Container Toolkit 并重启 Docker。

确认节点用上 Docker 后，把运行时固定下来：在**计算节点**中打开该节点，在**基础信息 → 运行时**里选择 `docker`。选择会保存，agent 重启或重连后不会改回自动探测；Docker 不可用时这一步会直接报错，而不是悄悄改用 runveil。

### WSL 上 agent 的补充配置

```bash
sudo mkdir -p /etc/systemd/system/gpuworks-agent.service.d

sudo tee /etc/systemd/system/gpuworks-agent.service.d/50-wsl-nvidia.conf >/dev/null <<'EOF'
[Service]
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/lib/wsl/lib
Environment=LD_LIBRARY_PATH=/usr/lib/wsl/lib
EOF

sudo systemctl daemon-reload
sudo systemctl restart gpuworks-agent
```

- `50-wsl-nvidia.conf`：保证 agent 能找到 WSL 透传的 `nvidia-smi` / CUDA 用户态库，避免 `gpu_count=0`。

控制台中节点应为 **在线**，且 GPU 数量 ≥ 1。

## 5. 准备 runner 镜像（可选预热）

部署时 agent 会拉取目录指定的 runner（例如 `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4`，以控制台实际选中为准）。

在中国大陆，若直连困难，可先从华为云 SWR 拉取再打标签（标签名与版本随发布变化，以当时部署规格为准）：

```bash
docker pull swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:<tag>
docker tag swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:<tag> \
  gpuworks/gpuworks-runner:<tag>
```

可选：在 runner 镜像内确认 CUDA 可用：

```bash
docker run --rm --gpus all --entrypoint nvidia-smi gpuworks/gpuworks-runner:<tag> -L
docker run --rm --gpus all --entrypoint python3 gpuworks/gpuworks-runner:<tag> \
  -c 'import torch; print(torch.cuda.is_available(), torch.cuda.get_device_name(0))'
```

## 6. 部署小模型并调用

1. 在控制台搜索并选择小模型模板（例如 Qwen3-1.7B）。
2. 选择面向 NVIDIA + vLLM 的规格（如 `bf16-vllm-nvidia-general`）。若提示 GPU 匹配到多个同等规格，必须显式指定规格名。
3. 权重源可按网络情况选择 Hugging Face 或 ModelScope 等。
4. 提交部署，等待状态变为可服务（首次拉镜像可能较久；同时关注 Windows 系统盘剩余空间）。
5. 创建推理 API 令牌，调用 OpenAI 兼容接口，见[从零启动并部署第一个模型](../tutorials/getting-started.md)与[OpenAI 兼容网关](../reference/openai-gateway.md)：

```bash
curl http://127.0.0.1:8800/v1/chat/completions \
  -H "Authorization: Bearer gw-…" \
  -H "Content-Type: application/json" \
  -d '{"model":"<served_name>","messages":[{"role":"user","content":"hello"}],"max_tokens":64}'
```

期望 HTTP 200，并返回 `choices`。

部署与选卡说明见[部署模型](deploy-models.md)。

## 7. 日常检查

| 检查项 | 命令或位置 | 期望 |
|---|---|---|
| WSL 发行版 | `wsl -l -v` | `Running` |
| 控制面 | `systemctl is-active gpuworks` | `active` |
| Agent | `systemctl is-active gpuworks-agent` | `active` |
| Docker | `systemctl is-active docker` | `active` |
| 运行时 | 节点详情**基础信息 → 运行时** | `docker` |
| 健康检查 | `curl http://127.0.0.1:8800/healthz` | `"status":"ok"` |
| 浏览器 | `http://localhost:8800` | 可打开控制台 |

## 8. 常见问题

| 现象 | 处理 |
|---|---|
| 浏览器打不开 `localhost:8800` | 看 `wsl -l -v` 是否 `Stopped`；补齐 `instanceIdleTimeout=-1` 后 `wsl --shutdown` 再进入 |
| 节点 `gpu_count=0` | 安装 `50-wsl-nvidia.conf` 后重启 agent |
| 部署失败 `Failed to infer device type` | 节点用的是 runveil。按第 4 节改用 Docker 并在**基础信息 → 运行时**固定为 `docker`，再重试部署单元 |
| Agent 日志仍是 udocker | 按日志里括号中的原因处理（见第 4 节）：没有 Docker 就 `systemctl start docker` 后重启 agent；不能使用 GPU 就配置 NVIDIA Container Toolkit 后重启 Docker |
| 拉镜像导致磁盘写满或 WSL 异常 | 先腾出系统盘空间；按第 2 节把 Docker 的 `max-concurrent-downloads` 设为 2 或 1 |
| Docker Hub / 镜像拉取超时 | 使用可用镜像源或 SWR 预拉后打标签 |

## 9. 验收清单

- [ ] `docker run --gpus all … nvidia-smi` 成功  
- [ ] `gpuworks`、`gpuworks-agent`、`docker` 均为 `active`  
- [ ] Agent 日志为 `driver=docker`，节点运行时已固定为 `docker`  
- [ ] 节点在线且 GPU 数量 ≥ 1  
- [ ] 小模型部署可服务  
- [ ] `/v1/chat/completions` 返回 200  
- [ ] Windows 浏览器可打开 `http://localhost:8800`  

全部满足即可认为 WSL2 + Docker 场景本机试用通过。
