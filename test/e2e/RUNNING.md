# 运行 E2E：依赖与最小可行路径

[README](README.md) 讲约定，本文只回答一个问题：**在我这台机器上要跑起来，需要装什么、
按什么顺序、缺东西时会卡在哪。** 内容来自对 248 个脚本的实际静态盘点与本机实测。

## 一、四层依赖，从轻到重

| 层 | 需要什么 | 覆盖脚本数 | 典型 case |
|---|---|---|---|
| L1 只用浏览器 | Python 3.12 + Playwright + 系统 Chrome | 71 | — |
| L2 加 server 二进制 | `bin/gpuworks` | 63 | `platform_token_read_revoke.py`、`inference_key_lifecycle.py` |
| L3 加 agent 二进制 | `bin/gpuworks-agent` | 25 | `deployment_replicas.py` |
| L4 加 Docker daemon | 可达的 docker socket + 已导入的镜像 | 18 | `playground_chat_gateway.py` |

（按 147 个可执行脚本统计，各层含上一层的脚本；`make build` 一次产出两个二进制。）

**先跑 L2 那批**：它们是"真实 Chrome + 隔离 server + SQLite"的纯本机链路，不碰 GPU、不碰容器，
覆盖平台令牌、推理密钥、网络存储、MCP 设置等主干功能。多数开发改动用 L2 就够了。

GPU（`nvidia-smi` / CUDA）只影响 7 个脚本，且这些脚本注入调度用的 GPU 事实、不认领真实算力。
真正的 GPU 推理验证不在本目录。

## 二、按顺序准备

```bash
# 0. 先问这台机器缺什么（不跑测试，只报告，缺依赖也不会失败）
make test-e2e-setup-check

# 1. Python 依赖与 Chrome
python3 -m pip install -r requirements-e2e.txt
google-chrome --version || chromium --version   # 必须存在；测试用 channel="chrome"，不下载 chromium

# 2. 构建二进制（一条命令产出 gpuworks 与 gpuworks-agent）
make build

# 3. 用最小链路证明环境可用（真实 Chrome + 隔离 server + SQLite，不碰 Docker/GPU）
make test-e2e-smoke
```

`make test-e2e-setup-check` 会逐项列出 `MISSING` 并给出对应的修复命令；Docker 缺失只作为 `NOTE`
提示，因为只有 Docker 那一层需要它。

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
make test-e2e-docker-check        # 缺 daemon 或镜像时直接告诉你缺哪个、怎么补
make test-e2e-playground-chat     # 该层入口；默认 daemon 不可达时用下面的写法
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

- 目录里 248 个 `.py` 的实际构成：**101 个是库/辅助模块**（无 `__main__`，如 `local_browser_fixture`、
  `dialog_checks`），其余 147 个可执行；其中 **93 个已封装成 `test-e2e-*` 目标**，可以直接
  `make test-e2e-<name>`，不用记 `NO_PROXY` 和模块路径。需要参数的用例（`--server-bin`、`--locale`、
  `--artifact-dir` 等）仍要手工调用，且各脚本 CLI 并不统一，用 `--help` 确认，不要假设。
- **`make build` 即使无改动也要约 2.5 分钟**（UI 打包 + 三平台 agent 交叉编译，全部是 `.PHONY`）。
  E2E 目标已改成依赖 `ensure-bins`：二进制在就复用（实测单条用例 39 秒），缺了才构建。
- ~~没有 `requirements.txt`~~ → 已加 `requirements-e2e.txt`；~~`docs/dev/setup.md` 未提及 e2e~~
  → 已补「浏览器 E2E」一节。
- 已有 `make test-e2e-setup-check`（依赖自检）与 `make test-e2e-smoke`（最小链路）两个入口。
- 每条 case 独立起自己的 server 和 SQLite、按 8800/8802… 选端口并加进程锁；**同一时间只跑一条浏览器 case**
  是最稳的用法。

## 六、想简化的话，优先级建议

1. ~~把 39 个裸 traceback 换成可操作提示~~ —— 已完成（共享 fixture 的 `require_binary()`）。
2. ~~加依赖声明并补 `docs/dev/setup.md`~~ —— 已完成（`requirements-e2e.txt` + setup.md 的 E2E 一节）。
3. ~~给 L2 补 Make 入口~~ —— 已有 `test-e2e-setup-check` / `test-e2e-smoke`，其余 L2 用例仍可直接调用。
4. ~~Docker 类（L4）单独成目标~~ —— 已完成：`make test-e2e-docker-check` 报告 daemon 与镜像是否就绪
   并给出 `make fake-engine-image`，`make test-e2e-playground-chat` 是这一层的入口。
