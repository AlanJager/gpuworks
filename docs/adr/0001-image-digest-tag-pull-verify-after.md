# ADR 0001: 镜像校验采用「tag 拉取 + 事后比对」而非 digest 不可变引用

日期: 2026-08-22 · 状态: 部分废止(2026-08-23,M20 验收 grill):image ID 三态校验(`expected_image_id`/VerifyState)因产出基准需实拉全部镜像、运营成本高且用户价值不明而移除;`blob_sha256` 离线导入校验保留。2026-10-08 镜像目录收窄删除了离线 tar 包导入，`blob_sha256` 比对随之不再生效（`artifacts/work/2026-10-08-image-catalog-simplify/plan-v2.md`），本 ADR 的校验部分至此全部废止。「ref 强制带 tag、不用 digest 引用」的决策本身仍然有效。原 grill 会话归档 `~/.grill_sessions/gpuworks-images-m19.json`。

## 背景

官方镜像目录需要校验「节点上的镜像是不是目录说的那个」。业界标准做法是 `image@sha256:<digest>` 不可变引用拉取。但本仓库的 canonical ref 从 M16.5 起强制带 tag、显式拒绝 digest(`imagecatalog/source.go`、`api/images.go` 校验),因为 tag 是 UI 展示、mirror 前缀改写、gpustack runner 编码 tag(`cuda12.8-vllm0.17.1`)整套机制的基础。

另有两个事实约束:
1. registry manifest digest 在多源转推(quay ↔ 阿里云等)下不稳定——部分转推工具会重组 multi-arch manifest list,digest 随之改变;
2. 离线 `docker load` 的镜像本地拿不到 registry manifest digest,只能拿到 image ID(config digest)。

## 决策

保持 ref 强制带 tag 不变。校验用独立的可选基准列,拉取/load 完成后事后比对:

- `expected_image_id`:目录提供,agent `docker inspect` 出 image ID 比对——同时覆盖在线拉取与离线导入两条路;
- `blob_sha256`:离线镜像 tar 的哈希,导入落盘时比对。

结果三态 verified / unverified / mismatch;mismatch 标红警告但不拦截部署。

## 后果

- (+) 多源与离线场景下校验语义一致,不受转推 digest 漂移影响;
- (+) 改动面小,tag/mirror/展示机制全部不动;
- (−) 不是拉取时强制,理论上存在「拉下来才发现不对」窗口——以导入时校验+部署时读状态缓解;
- (−) image ID 校验依赖目录生成工具产出正确基准,生成工具须与镜像同源构建/实拉产出。

被否方案:`@sha256:` 不可变引用(推翻 tag 体系、多源 digest 漂移)、部署前每次重算(重复开销,无新信息)。
