# 发布指南

## 构建产物

以下命令在仓库根目录执行。先构建 UI 等前置产物，再生成用于分发的压缩包：

```bash
make build
make release-packages
```

`make build` 先校验资产、构建 UI 与跨平台 agent，再构建 server 和当前平台 agent 到
`bin/`。server 通过 `go:embed` 携带 `internal/server/webui/dist` 和各目标平台 agent；
发布时不能只重编 server 而跳过这些前置产物。Linux amd64 单目标可用
`make server-linux-amd64`，但仍会构建 UI 和被嵌入的 agent。

`make release-packages` 产出 Linux 与 macOS 的 amd64/arm64 四个 server 压缩包（各约 45 MB）和统一的
`SHA256SUMS`。server 内嵌的三个 agent 以 zstd 压缩存放（构建机有 `zstd` 命令时用 `-19`，release job 会先尝试
`apt-get install zstd`，失败则退回 Go 编码器，包会增大约 1.5 MB），解压后的 server 二进制约 69 MB。macOS server 同样内嵌 Linux amd64/arm64 agent，方便在开发机运行控制面并
接入 Linux GPU 节点；Apple silicon Mac 节点通过 `host` 运行时以宿主机进程运行受支持引擎，能力边界见 [ADR 0044](../adr/0044-mac-engines-as-host-processes.md)。

每个压缩包里除 `gpuworks` 二进制与 `INSTALL.txt` 外，还带 `LICENSE`、`NOTICE` 与 `LICENSES/`：二进制内嵌的 PRoot、talloc、perftest 等第三方程序
要求分发二进制时附上许可证正文。新增或更换内嵌资产时，同步更新 `NOTICE` 与 `LICENSES/`。

CI 为避开单 job artifact 大小限制（server 内嵌了 macOS agent 后单个压缩包约 53 MB，
一个 job 放两个就超限），按 OS 与架构拆成四个 job：`release-build-linux-amd64`、
`release-build-linux-arm64`、`release-build-macos-amd64`、`release-build-macos-arm64`，
各只打一个压缩包，可用 `GPUWORKS_RELEASE_OSES=linux|darwin` 与
`GPUWORKS_RELEASE_ARCHES=amd64|arm64` 在本地复现。发布站只合并同一 pipeline 中全部成功的
四份产物。

两个运行二进制必须保持 `CGO_ENABLED=0`。发布前检查：

```bash
make check-assets          # HAMi libvgpu.so 的 glibc 基线，以及 ib_write_bw 的库/符号基线与哈希
make runtime-assets-verify # 昇腾 vCANN、PPU HGGC 与 partition probe 的清单完整性 + 逐文件 sha256
make test
make vet
git diff --check
```

`check-assets` 包含 `check-libvgpu` 和 `check-perftest`，不校验昇腾、PPU 或 partition probe 资产；
发布前必须单独运行 `make runtime-assets-verify`，否则这三类资产的缺失或篡改不会被发现。

## 版本号

正式版本用日历版本号 `YY.M.PATCH`，tag 写作 `vYY.M.PATCH`，例如 `v26.11.0`。

- `YY.M` 是从 master 切出发布分支的年月，月份不补零（`26.1.0`，不写 `26.01.0`）。一条发布线的首个版本 `PATCH` 为 0，
  之后只修缺陷的补丁版本依次加一。
- 每条发布线一个分支 `release/YY.M`，从 master 上选定的提交切出。分支上只合缺陷修复：修复先合进 master，再
  `git cherry-pick -x` 到发布分支。
- 打 tag 前，在 `docs/releases/` 新增该版本的发布说明（`vYY.M.PATCH.md`，含升级须知、新增、变更、修复、安全与下载校验和），
  在 `docs/releases/README.md` 的版本列表顶部加一行，并同步 `docs-en/releases/` 的译文；先合入 master，再回合到发布分支。
- 在发布分支上打带注释的 tag 并推送（`git tag -a v26.11.0 -m "GPUWorks 26.11.0"`、`git push origin v26.11.0`）。
  只有匹配 `^v[0-9]+\.[0-9]+\.[0-9]+$` 的 tag 会创建 pipeline 并发布到 `release` 渠道；`v26.11.0-rc.1` 这类 tag 不创建 pipeline。
- `v*` 必须在 GitLab 里设为 protected tag：签名私钥和发布源凭据是受保护变量，不受保护的 tag 的 pipeline 读不到它们，发布 job 会失败。
- tag 推送后不移动、不重打：一个 release id 只对应一份字节（见下文发布流程第 1 步），有问题就发下一个补丁版本。
- server 判断有没有更新时，两边都是版本 tag 就按版本号比较，否则（nightly 的短提交号、本地构建）按构建时间比较
  （[ADR 0040](../adr/0040-signed-multi-source-distribution.md)）。`release` 渠道只跟随最新一条发布线：渠道已经指向更高版本时
  （例如 `v26.12.0` 发布之后再推 `v26.11.2`），`publish_dist.sh` 拒绝发布。

## 发布通道

server 版本、系统镜像目录与内置推理模板目录通过同一个发布通道交给已安装的 server（[ADR 0040](../adr/0040-signed-multi-source-distribution.md)）。
三个发布源放逐字节相同的目录树：

| 源 | 地址 | 用途 |
|---|---|---|
| 阿里云 OSS 上海 `gpuworks-dl` | `https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com` | 中国大陆（默认域名，免备案） |
| 华为云 OBS 上海一 `gpuworks-dl` | `https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com` | 中国大陆（默认域名，免备案） |
| Cloudflare R2 `gpuworks-dl` | `https://dl.gpuworks.ai` | 其他地区 |

三个源使用相同的目录结构。`<channel>` 表示发布渠道，`<release_id>` 标识一份发布件，
`<sha256>` 是对应目录文件的内容摘要：

| 路径 | 内容与用途 |
|---|---|
| `<channel>/index.json` | ed25519 签名信封，即带签名的发布索引；正文列出 server 各平台压缩包、镜像目录与推理模板目录包，每个文件带路径、大小与 sha256 |
| `<channel>/latest` | release id 纯文本，供首次下载定位版本 |
| `<channel>/manifest.json` | 与该渠道索引里的镜像目录是同一文件，固定地址，供离线环境手工下载后上传 |
| `releases/<release_id>/` | server 压缩包与 `SHA256SUMS` |
| `catalog/images/<sha256>.json` | 按内容摘要标识的镜像目录 |
| `catalog/templates/<sha256>.tar.gz` | 按内容摘要标识的推理模板目录包 |
| 根目录 `get.sh` | 一键安装脚本（源文件 `scripts/get.sh`）：挑最快的源，下载本机平台的压缩包、按 `SHA256SUMS` 校验后运行 `gpuworks install` |

渠道 `nightly` 是每次 master 构建，release id 是短提交号；`release` 是每个版本 tag，release id 就是 tag（见[版本号](#版本号)）。
二进制跟随构建时注入的渠道（`make CHANNEL=…`，默认 `nightly`；`scripts/package_release.sh` 读 `GPUWORKS_RELEASE_CHANNEL`，
`release` 渠道只接受 `vYY.M.PATCH` 形式的 release id）。

CI 的 `release-publish` 在 master 或版本 tag 的四个构建 job 全部成功后运行 `scripts/publish_dist.sh <渠道> dist`。
它位于独立的 `publish` 阶段，因为 GitLab 14.0 的 needs 只能指向更早阶段的 job；
`make check-ci-yaml` 检查这一约束。脚本执行以下流程：

1. 一个 release id 永远只对应一份字节：`releases/` 下的文件按 immutable 长缓存，CDN 留着它第一次见到的那份。同一提交再次构建
   （新流水线、手工重跑）产出的包字节不同（构建时间写在包里），所以某个源已有 `releases/<id>/` 时，脚本把它拷回来，
   `gpuworks-dist adopt` 核对与 `SHA256SUMS` 一致后，发布的是这份已发布的包与它的构建时间，丢弃本次构建的包；
2. 如果某个源已经在提供同一渠道中创建时间更晚的构建，说明流水线乱序完成，本次不再发布任何内容；`release` 渠道另外按版本号比较，
   某个源的 `release/latest` 是更高版本时报错退出；
3. `gpuworks-dist assemble` 用这些压缩包和本次检出内嵌的两份目录生成目录树并签名索引；
4. rclone 以 `--immutable` 把不可变文件推到三个源：源上同名对象内容不同时报错退出，绝不覆盖（这种源要人工处理，见下）；
5. 切换前，`gpuworks-dist verify -index` 用新索引逐个核对三个公开地址（含 CDN）上的文件大小，任一不符就停下，通道仍指向上一版；
6. 先存下各源当前的索引、`latest` 与 `manifest.json`，再切换到新版本，并从三个公开地址读回核对；切换或读回失败时把各源恢复到原来的指针后退出；
7. 把 `scripts/get.sh` 以 `Cache-Control: no-cache` 传到每个源的根目录 `get.sh`；
8. 版本 tag（`vYY.M.PATCH`）的目录在每个源上永久保留；nightly 目录每个源只保留最近 30 个（`GPUWORKS_DIST_KEEP`），以及两个渠道 `latest` 指向的版本。

源上的 `releases/<id>/` 与它自己的 `SHA256SUMS` 对不上（例如本规则之前被覆盖过）时，发布会在第 4 步失败而不切换。处理方式：确认哪个源上的
那份是对的，把错的那份删掉或换成对的，并清掉 CDN 上 `releases/<id>/` 的缓存，再重跑发布。

凭据与签名私钥是受保护、掩码的 CI 变量：`GPUWORKS_DIST_SIGNING_KEY`（base64 的 32 字节 ed25519 种子）、`GPUWORKS_DIST_SIGNING_KEY_ID`、
`GPUWORKS_DIST_OSS_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY`、`GPUWORKS_DIST_OBS_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY`、
`GPUWORKS_DIST_R2_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY` / `GPUWORKS_DIST_R2_ENDPOINT`。三组存储凭据各属于一个只能读写
`gpuworks-dl` 里对象的专用身份（阿里云 RAM 用户与华为云 IAM 用户 `gpuworks-dist-ci`、策略 `gpuworks-dist-publish`，
R2 账户令牌 `gpuworks-dist-ci`），不能建桶，所以两个发布脚本给 rclone 设了 `no_check_bucket`。

本地手工补发用 `make dist-publish CHANNEL=nightly`，
凭据放 `secrets/dist-publish.env`。发布 release：`make release-packages CHANNEL=release GPUWORKS_RELEASE_ID=v26.11.0` 后
`make dist-publish CHANNEL=release`。

Cloudflare 区域 `gpuworks.ai` 上为 `dl.gpuworks.ai` 配了两条缓存规则：`*/index.json`、`*/latest`、
`*/manifest.json` 一律不缓存；404 不缓存（否则发布前 `newer` 检查拿到的 404 会被边缘按默认 4 小时缓存，新渠道首次发布后读不到）。
不可变文件按来源的 `Cache-Control: immutable` 长缓存。`/notices.json`（发布方通知，见下）与 `/get.sh` 也必须不缓存，需要在同一条规则里加上。

发布方通知（[ADR 0054](../adr/0054-usage-report-and-notices.md)）：编辑 `build/notices/notices.json`（未签名的正文，schema 与校验见
`internal/server/distribution/notices.go`：每条有 `id`、`severity`、`kind`、至少英文的 `title`、可选 `body` 与 https `url`、必填 `expires_at`，
可用 `when` 限定渠道、server 构建时间区间与加速卡厂商），然后运行 `scripts/publish_notices.sh`：它按 server 的规则校验、用发布密钥签名、
以 `Cache-Control: no-cache` 上传到三个源的根目录，再从每个公开地址读回核对。通知与构建无关，随时可以单独发布；撤回就删掉那一条再发布一次。

改进计划日报的收集端（[ADR 0054](../adr/0054-usage-report-and-notices.md)）：阿里云 SLS 上海 project `gpuworks-usage`、logstore `reports`
（WebTracking 开启、`appendMeta` 开启即记录来源 IP `__tag__:__client_ip__`，保存 180 天，`schema`/`install_id`/`day`/`version`/`report`/`__tag__:__client_ip__` 建分析索引，`report` 上限 16 KiB），
用 SQL `json_extract_scalar(report, '$.usage.routes["GET /nodes"]')`、`ip_to_province("__tag__:__client_ip__")` 之类取字段；兜底是 Cloudflare Worker `gpuworks-usage`
（自定义域名 `usage.gpuworks.ai`，代码在 `scripts/usage-collector/`，`node --test scripts/usage-collector/worker.test.mjs`），
报告存进私有 R2 桶 `gpuworks-usage` 的 `reports/<day>/<install_id>.json`，来源 IP、国家、地区在对象元数据 `client_ip`/`country`/`region`。WebTracking 实际只接受 `__topic__`/`__source__`/`__logs__`/`__tags__`
四个顶层键，与其 API 文档列出的 `project`、`logstoreName` 不符。

**签名密钥轮换**：生成新密钥，把新公钥加进 `internal/server/distribution/defaults.go` 的 `trustedKeys`（与旧钥并存）发一个版本，
等装机量升过来后再改用新私钥签名，下一个版本删掉旧公钥。

新建桶时注意两家对象存储默认打开桶级「阻止公共访问」，要关掉并设为公共读。

## 内嵌原生资产

内嵌原生资产包括下表中的运行库、工具和探针镜像。它们不是普通 Go 源码，
更新后不能只重编 server 就带上，需按各自入口校验或重建：

| 资产 | 文件 | 平台 | 校验/重建 |
|---|---|---|---|
| NVIDIA HAMi | `internal/assets/bin/libvgpu.so` | 单份，最老受支持 glibc 基线编译 | `make check-libvgpu`（`scripts/check_libvgpu_compat.sh`） |
| RDMA 带宽测试工具 | `internal/assets/bin/ib_write_bw-{x86_64,arm64}` | x86_64 + arm64，运行时使用宿主机 RDMA 用户态库 | `make check-perftest`（`scripts/check_perftest_compat.sh`）；构建说明见 `build/runtime-assets/perftest/README.md` |
| 昇腾 vCANN | `internal/assets/bin/runtime-assets/ascend/linux-{amd64,arm64}/libvruntime.so` | amd64 + arm64 各一份 | `make runtime-assets-verify`；重建用 `make runtime-assets` |
| PPU HGGC | `internal/assets/bin/runtime-assets/ppu/linux-amd64/hggc-13.0/{hggc_quota.so,hgml_dlsym_hook.so}` | 目前仅 amd64 | 同上 |
| partition probe | `internal/assets/bin/runtime-assets/probe/partition-probe-linux-{amd64,arm64}.tar.gz` | amd64 + arm64 | 同上；镜像单独用 `make runtime-assets-probe` 重建 |
| udocker PRoot | `internal/assets/bin/proot-{x86_64,arm64}-static-nofd` | x86_64 + arm64 | `check-assets`/`runtime-assets-verify` 均不覆盖，需按 `internal/assets/bin/manifest.sha256` 人工核对 |

昇腾与 PPU 运行库的清单和哈希集中在 `internal/assets/bin/runtime-assets/manifest.json`，backend id
分别是 `ascend-dgpu`、`ppu-dgpu`（定义于 `internal/accelerator/backend.go`）。partition probe 的哈希
在各压缩包旁的 `.tar.gz.sha256` 文件中；`make runtime-assets-verify` 同时检查运行库清单、probe 哈希与镜像归档结构。
更换任意资产
都必须保留来源、架构、哈希和许可证记录，运行对应资产检查，并确认 release package 实际包含新文件。
各厂商发布资产只支持声明过的平台组合（PPU 当前无 arm64 构建）；用户侧限制见
[dGPU 支持矩阵](../reference/support-matrix.md)。

## 数据库与目录包

server 启动会执行版本化迁移；发布前必须验证 migration marker（记录迁移版本的标记）、重复执行的幂等性和事务回滚。
内置推理目录是内容寻址包，即以内容摘要标识包；发布构建应对包做同一套 schema、路径和摘要校验。系统镜像目录
由 `make image-manifest` 从 gpuworks-runner 构建矩阵生成内嵌种子，随每次发布进入[发布通道](#发布通道)，见
[CLI 参考](../reference/cli.md#gpuworks-image-manifest)；内嵌种子与矩阵不一致时单测失败。

## 升级与回滚

- server：`gpuworks upgrade` 自动完成停服、备份（数据库 + `master.key` + 旧二进制）、原子替换、
  探活与失败回滚；手工路径备份数据目录和 `master.key` 后原子替换并重启。两条路径回滚前都要
  确认数据库迁移是否向后兼容。
- agent：从节点页升级或重跑 `/install.sh`。下载先进入临时文件，成功后原子替换；连接 smoke
  失败不应覆盖当前可执行文件。
- 记录发布版本、构建 commit、平台、内嵌资产摘要和目录包 digest。一次性验收结果放
  `artifacts/work/<YYYY-MM-DD>-<主题>/`（收口后随主题搬进 `artifacts/archive/`）。
