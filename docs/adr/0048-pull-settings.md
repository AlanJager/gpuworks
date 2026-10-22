# ADR 0048: 镜像从哪里拉由节点的拉取设置决定，部署不再选来源

日期：2026-10-16 · 状态：已接受（取代 [ADR 0018](0018-explicit-deployment-acquisition.md) 中「部署显式选择获取方式」的部分与 [ADR 0038](0038-template-image-sources.md) 决策 5「系统改写规则只作用于系统镜像」；ADR 0038 其余决策保留）

依据：[镜像与模型拉取设置计划](../../artifacts/work/2026-10-16-image-pull-policy/plan.md)。

## 背景

镜像来源原由五处决定：部署页的获取方式与网络路径、全局 `image_registry_rewrites`、镜像目录的 `registry_sources`、模板的 `image_sources`、节点的 `network.rewrites`。四条拉取路径（部署启动、部署无记录时的默认链、镜像页拉取、多机预拉取）各算各的：

- 部署记录的是「当时对那台节点的获取方式」。选了「本地已有」或「节点间同步」的部署扩副本到新节点时，候选退化成 `[原名]`（Docker Hub）且关闭节点改写与代理；选了「节点间同步」或「归档 URL」的扩到新节点会一直等一个不会到达的镜像。
- 多机预拉取走全局默认链，不看部署选的来源，也不看模板来源。
- 部署页不读全局设置，第三方模板镜像只有 Docker Hub 一个候选。
- 内置的第三方 Docker Hub 加速器（DaoCloud 有白名单与 1 MB/s 限速，高校站 2024 年停服，阿里云个人加速器只对阿里云 ECS 生效）不可靠。
- 管理网默认承担存储同步，「两节点有共同存储同步网络」几乎总成立，不能据此判断能否同步；云容器（RunPod、Vast、AutoDL）之间通常不互通。

## 决策

1. **三类事实，各归其主。** 镜像的等价副本归镜像（模板 `image_sources`、目录 `registry_sources`、原名，`pullref.Copies`）；节点能从哪里拉归管理员的拉取设置；部署只记录要运行什么。
2. **来源规则**（`pull_source_rules`，按位置排序）：匹配一个仓库 host 或引用前缀，给出有序替代地址，可关闭回源，可按节点标签限定作用范围。语义同 containerd `hosts.toml`：先依次试替代地址，失败或卡住就试下一个，最后回到原地址（除非关闭回源）；拉到后在本地 tag 回原名。规则只在 agent 侧以「拉另一个地址」实现，不改 `daemon.json` / `hosts.toml`，因此只作用于平台发起的拉取。
3. **同一份候选，一处计算。** `pullref.PlanCandidates`：适用于该节点的规则产生的替代地址在前（规则顺序），镜像自己的副本在后并标 `rank`；任一匹配规则禁止回源时丢弃该副本本身；什么都不剩即报错。`planning.PullCandidatesFor` 是启动、期望状态重放、镜像页拉取、多机预拉取的唯一入口。实例持久化副本（`image_sources`），候选在下发时按当时的规则重算。
4. **节点自己排序副本。** agent 并行探测 `https://<host>/v2/`（5 秒超时，缓存 30 分钟，走与拉取相同的代理），`rank` 段内按可达、延迟排序；不记录下载速度。下载阶段 3 分钟没有新增字节视为卡住，换下一个候选；失败或卡住的端点在该节点冷却 30 分钟，冷却中排末尾不剔除。慢但在动的不中途切换。
5. **同步组**（`sync_groups`）：节点只自动从同组节点复制镜像与模型；新装与升级都预置一个选择全部节点、名为 `default-all-nodes` 的普通组（没有“默认”标记，可改可删）；不属于任何组的节点不自动复制；多组取并集，限速取共享组里最严的。能否连通不靠推断：直连失败的节点对在 server 内存冷却 3 小时，复制失败后实例自行从仓库拉取（模型则下载）。同步组只表达「连得上但不该走」。租机节点带系统标签 `gpuworks.ai/provider`、`gpuworks.ai/region`（由租机记录派生）。
6. **部署不再选来源。** 部署请求只保留保存模型的存储与权重来源（运行哪份权重）；不接受镜像获取方式、源节点、网络路径。评估对请求指定的节点给出预估（已有 / 从某节点复制 / 按候选拉取），是预估不是选择。镜像页手动拉取同样走自动候选，不能指定仓库；指定源节点的手动复制保留。
7. **仓库凭据**（`registry_credentials`，secretbox 加密）按 host 保存，随 state.sync 下发，agent 只在内存保存；拉取时写临时 `DOCKER_CONFIG`，拉完删除，不执行 `docker login`。
8. **docker 节点的代理由平台写进 dockerd。** 节点代理（全局与节点覆盖，新增 `no_proxy`）对 nerdctl / udocker 按次生效；docker 由 agent 写 `/etc/systemd/system/docker.service.d/zz-gpuworks-proxy.conf` 并 `daemon-reload`，不重启。agent 上报 dockerd 实际代理、live-restore、`daemon.json` 是否也配了代理、权限（root / 免密 sudo / 无）；不一致时界面提示未生效，「重启 dockerd」需用户确认（`runtime.restart`）。无权限时只读并给出命令。不经 SSH。
9. **迁移**：`image_registry_rewrites` 里用户自加的规则、`network` / `network:<id>` 里非 URL 的改写转成来源规则（节点覆盖以 `gpuworks.ai/node` 限定），原设置删除；原设置含内部仓库 registry.zstack.ai 时生成 `gpuworks/ → registry.zstack.ai/gpuworks/`；部署快照里的 `acquisition` 记录删除。镜像目录种子不再列内部仓库。

## 取舍

- **放弃「部署锁定来源」**：同一部署的副本可能从不同来源拉到同一 tag 的不同内容。不按 digest 拉（digest 可能被回收，副本间 manifest digest 也可能不同），改为在部署详情逐副本显示镜像 ID 并提示不一致。
- **探测只判可达**：`/v2/` 与下发数据的 CDN 不是同一处，探测快不代表下载快；由卡住判定与冷却兜底。docker CLI 只向终端输出字节进度，agent 因此在伪终端里运行 `docker pull`；分配不到伪终端时退回管道，此时卡住判定不生效，只受单候选总时长上限约束（含解包，2026-10-20 由 30 分钟改为 60 分钟）。
- **新增持久化事实**：规则、同步组、凭据都是管理员的配置，无法推导；全部用带类型的表。dockerd 观测值与冷却状态不落库。
- **规则不改宿主机运行时配置**：节点上手动 `docker pull` 不走规则，换取不需要重启运行时、不影响宿主机其他业务。

## 后果

- 新增 `/api/v1/pull/source-rules`、`/api/v1/pull/sync-groups`、`/api/v1/pull/registry-credentials`、`POST /api/v1/nodes/{uuid}/runtime/restart`；`PUT /settings/image_registry_rewrites` 等返回 410。
- agent 新增能力 `image.pull.candidates`：`image.pull` 一次收下整条候选，自己回退；旧 agent 仍由 server 逐个下发。
- 模型下载的来源规则（替代节点 URL 改写里的 Hugging Face 镜像站）与「禁用 Xet」是后续一步，本 ADR 只覆盖镜像与两者共用的同步组。
