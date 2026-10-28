# 运行 E2E：依赖与最小可行路径

[README](README.md) 讲约定，本文只回答一个问题：**在我这台机器上要跑起来，需要装什么、
按什么顺序、缺东西时会卡在哪。** 内容来自对 248 个脚本的实际静态盘点与本机实测。

## 一、四层依赖，从轻到重

| 层 | 需要什么 | 覆盖脚本数 | 典型 case |
|---|---|---|---|
| L1 无需构建产物 | Python 3.12 + Playwright + 系统 Chrome | 135 | — |
| L2 加 server 二进制 | `bin/gpuworks`（`make build` 产出） | 57 | `platform_token_read_revoke.py`、`inference_key_lifecycle.py` |
| L3 加 agent 二进制 | `bin/gpuworks-agent`（同样是 `make build`） | 35 | `deployment_replicas.py` |
| L4 加 Docker daemon | 可达的 docker socket + 已导入的镜像 | 27 | `playground_chat_gateway.py` |

**先跑 L2 那批**：它们是"真实 Chrome + 隔离 server + SQLite"的纯本机链路，不碰 GPU、不碰容器，
覆盖平台令牌、推理密钥、网络存储、MCP 设置等主干功能。多数开发改动用 L2 就够了。

GPU（`nvidia-smi` / CUDA）只影响 17 个脚本，且这些脚本注入调度用的 GPU 事实、不认领真实算力。
真正的 GPU 推理验证不在本目录。

## 二、按顺序准备

```bash
# 1. Playwright 与 Chrome（无 requirements.txt，需手动安装）
python3 -m pip install --user playwright
# 系统 Chrome 由 channel="chrome" 直接使用，Playwright 不需要下载 chromium
google-chrome --version || chromium --version   # 必须存在

# 2. 构建二进制（一条命令产出 gpuworks 与 gpuworks-agent）
make build
ls bin/gpuworks bin/gpuworks-agent

# 3. 跑一条最轻的 case 验证链路
NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost \
  python3 test/e2e/platform_token_read_revoke.py
```

`make build` 的前置里有两个容易踩的坑：

- `check-libvgpu` 需要 `objdump` 与仓库内的 `internal/assets/bin/libvgpu.so`（9.1M，已入库，非 LFS）。
  缺 `objdump`：`apt install binutils`。
- `cross-agents` 会交叉编译所有平台。**不要导出 `GO`/`CGO_ENABLED`/`GOTOOLCHAIN`/`GOMAXPROCS`**：
  这些变量会污染该循环里的 `$(GO)` 展开，症状是 `/bin/sh: 4: build: not found`，
  与真实的编译错误毫无关系，极难定位。需要指定工具链时用 `make ... GO=<绝对路径>` 传参。

## 三、Docker 相关（L4）

`playground_chat_gateway.py` 这类脚本需要一个可达的 docker socket 和本地引擎镜像
（`gpuworks/fake-engine:test`）。镜像由 `make fake-engine-image` 构建。

它们支持 `DOCKER_HOST`，所以可以指向任意私有 daemon：

```bash
DOCKER_HOST=unix:///path/to/docker.sock python3 test/e2e/playground_chat_gateway.py --locale en
```

浏览器类 case 建议显式设置 `TMPDIR`（某些沙箱里 `/tmp` 的 noexec 或空间不足会让 Chrome 起不来，
报错与 TMPDIR 无关）。

## 四、缺依赖时会看到什么

好消息：缺二进制时脚本返回 `BLOCKED_INFRA`，不会崩成一个含糊的失败。

坏消息：**45 个会报 `BLOCKED_INFRA` 的脚本里，39 个只给一段裸 Python traceback**，
没有"请先 `make build`"之类的话。你大概率会看到：

```
FileNotFoundError: [Errno 2] No such file or directory: '/path/bin/gpuworks'
```

看到 `FileNotFoundError` 指向 `bin/` 或 `bin/gpuworks-agent`，就是没执行 `make build`（或它中途失败了）。
看到 Playwright 的 `Executable doesn't exist` / `Target page closed`，先查 Chrome 是否存在。

## 五、当前结构与已知摩擦

- **248 个脚本，只有 6 个 Make 入口**（`test-e2e-platform-token`、`-inference-key`、`-network-storage`、
  `-mcp-settings`、`-local`、`-log-filter-export`）。其余需要手工调用并自己给参数，各脚本 CLI 并不统一
  （`--server-bin`、`--artifact-dir` 等并非都有）。用 `--help` 确认，不要假设。
- **没有 `requirements.txt` / `pyproject.toml`**，Python 依赖没有版本约束，也没有安装脚本。
- `docs/dev/setup.md` 未提及 Python / Playwright / Chrome / e2e，而 README 却指向它要"开发环境配置"。
- 每条 case 独立起自己的 server 和 SQLite、按 8800/8802… 选端口并加进程锁；**同一时间只跑一条浏览器 case**
  是最稳的用法。

## 六、想简化的话，优先级建议

1. **给 39 个裸 traceback 换成一句可操作提示**，收益最大、改动最小（只改异常分支，不动断言）。
2. **加 `requirements.txt` 并在 `docs/dev/setup.md` 里补 e2e 一节**，让新人按文档能走到 L2。
3. **给 L2 那 57 个脚本补 Make 入口**（很多已有现成 CLI），让"不用记参数"成为默认。
4. Docker 类（L4）单独成一个显式目标，并在缺镜像时直接说 `make fake-engine-image`。
