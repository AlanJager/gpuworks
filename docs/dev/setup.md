# 本地开发环境

## 依赖

- Go 版本以 `go.mod` 为准（当前为 Go 1.26.9，与 CI 镜像 `golang:1.26` 一致）。
- Node.js 与 npm；前端依赖版本以 `ui/package.json` 和 lockfile 为准。
- GNU Make。构建或 L3/L4 测试还可能需要 Docker、SSH、`curl` 和目标厂商 CLI。

```bash
go version
node --version
npm --version
make demo
```

本地 server 默认监听 `:8800`、数据写入 `./data`。并行开发实例依次使用 8800、8802、
8804、8806；测试密码使用 `password`，不要把测试密码用于生产。

## 分开启动后端与前端

```bash
make run-server
cd ui
npm install
npm run dev
```

正式 `make ui` 会先做 i18n 检查，然后把产物写入 `internal/server/webui/dist`。前端开发
服务器只用于热更新；验证嵌入产物时应运行 `make ui` 后再启动 Go server。

## 本地秘密

地址、SSH 参数和云 key 只放在被 gitignore 的 `secrets/`。可创建
`secrets/test-environments.md` 记录本机环境，但不要提交真实值。环境变量的非敏感说明见
[配置参考](../reference/configuration.md)。

## 部署到长期验收环境

长期运行的验收 server 与真机（地址、SSH 方式、账号）记录在被 gitignore 的 `secrets/test-environments.md`，
不入仓库。更新这类 server 用 `gpuworks install` 重新安装同一个目录：它会复制自身、重写 service 并探活，
不需要手写 systemd 单元。

```bash
make server-linux-amd64          # 带 ui 和内嵌 agent，不要用 go build 代替
./bin/gpuworks-linux-amd64 install --dir /opt/gpuworks/server --addr :8800
curl --fail http://127.0.0.1:8800/healthz
```

注意 `install` 不做备份（只有 `upgrade` 会备份）：替换前先自己复制一份旧二进制
`/opt/gpuworks/server/gpuworks`，想回滚时从目标 commit 重新构建再 install。部署后若服务启动失败，
先看 `systemctl status gpuworks` 和 `journalctl -u gpuworks`。

## 常用检查

```bash
make test
make test-integration
make vet
git diff --check
```

更完整的分层、race、build-tag 和真机要求见[测试指南](testing.md)。
