# 离线环境部署

**适用场景**：集群不能访问公网或公网 registry，需要用内网镜像仓 / HTTP 文件服务器上的镜像归档和本地模型目录完成部署。

**前置条件**：

- server 与节点已装好，见[安装和升级控制面](install-server.md)与[接入节点](connect-nodes.md)。
- 内网有镜像仓（registry 镜像）或节点能访问的 HTTP 文件服务器，用来放镜像；模型目录已在节点本地，或使用离线模型包。
- 管理员权限（更新镜像目录、改设置）。

1. **更新官方目录（可选）。** server 自带内嵌的系统镜像目录与推理模板目录，不需要联网。要换成更新的目录，
   在能联网的机器上从任一发布源（如 `https://dl.gpuworks.ai`）下载 `manifest.json` 与 `<channel>/index.json` 里列出的
   `catalog/templates/<sha256>.tar.gz`，拷进内网后在设置页「版本与更新」点「上传 manifest」「上传目录包」
   （等同 `POST /api/v1/images/import` 与 `POST /api/v1/inference-templates/official`）。目录规则见
   [镜像目录](../reference/image-families.md#更新目录)。离线环境里「检查更新」会因为发布源不可达返回
   502 `UPDATE_SOURCES_UNAVAILABLE`，不改库。
2. **让节点拿得到镜像，二选一或组合：**
   - **内网镜像仓**：把需要的镜像同步进内网 registry，在设置 → 拉取设置 → 来源规则加规则，例如
     `gpuworks/` → `registry.corp.example/gpuworks/`，并关闭「允许回源」，节点不会再去公网拉；
     仓库要登录时配[凭据](configure-pull-settings.md#凭据)。规则写法见[配置镜像拉取](configure-pull-settings.md#来源规则)。
   - **镜像归档 URL**：在联网环境 `docker save` 出 tar，放到节点（或 server）能访问的 HTTP 地址，再按
     [从 HTTP 文件服务器导入](import-from-http.md#从-url-导入镜像归档)在镜像页把 URL 登记为该镜像版本的获取地址
     （系统镜像版本同样可以追加，目录更新时保留），在节点行手动「从 URL 导入」：节点直接下载，或选「管理节点中转」由 server 流式推送。部署不会自己选归档 URL，先导入再部署。
3. **分发到其他节点。** 一台节点就位后，同步组内的其他节点启动副本时会自动从它复制；也可以用[节点间同步](peer-sync-models.md)手动分发。agent 流式导入，
   只有导入完成且 `ImageExists` 为真才显示 `present`。
4. **部署。** 注册节点上的模型目录（或用离线模型包），部署，节点优先用本地已有的镜像，验证 `/health` 与 `/v1`。

## 节点不能出公网

常见于云上没有公网 IP、也没有 NAT 网关的实例（例如阿里云 VPC 里的 ECS）：agent 能连上 server，
但节点下载不了 Docker Hub 上的镜像，也下载不了 Hugging Face / ModelScope 上的模型。这时镜像和模型按下面的办法导入，
前提是 agent 已经接入（节点连得上 server，见[节点与 server 网络不通](connect-nodes.md#节点与-server-网络不通)）。

**镜像**，任选其一：

- **经管理节点中转**：节点本身什么都访问不到时也能用。在联网环境 `docker save` 出镜像归档，放到 **server**
  能访问的 HTTP 地址（例如 server 所在主机上的 nginx 目录、内网文件服务器），在镜像详情页的节点行里
  「从 URL 导入」并勾选「管理节点中转」：server 边下载边推给节点，不落盘。步骤见
  [从 HTTP 文件服务器导入](import-from-http.md#节点获取方式直连或管理节点中转)。
- **内网镜像仓**：把镜像同步到节点能访问的镜像仓，再加一条来源规则（见上文第 2 步）。
  阿里云上可以用容器镜像服务 ACR，节点通过它的 VPC 地址（`registry-vpc.<地域>.aliyuncs.com`）拉取，不需要公网。
- **节点间同步**：已有一台节点有这个镜像时，用[节点间同步](peer-sync-models.md)复制过去。这要求两台节点之间能直连。

**模型**，任选其一：

- **共享存储加扫描导入**：在节点能访问的 NFS 上放好模型（阿里云上可以用同 VPC 的 NAS），在「存储」页添加 NFS
  存储，点「扫描导入」登记，见[导入已有的模型库](storages.md#导入已有的模型库)。之后各节点挂载即用。
- **拷到节点本地后登记**：用任何能到达节点的方式把模型目录放到节点上（例如阿里云上用 `ossutil` 经 OSS 内网地址
  `oss-<地域>-internal.aliyuncs.com` 下载，不需要公网；`ossutil` 本身的安装包在公网上，要先拷到节点），再在模型页「注册模型」，来源选「计算节点本地」，填目录路径。
- **登记路径**：模型目录拷到节点后，也可以先登记 Hugging Face / ModelScope 模型，再在该节点「登记路径」，会补齐元数据，细节见[离线导入模型](configure-pull-settings.md#离线导入模型)。
- **内网 HTTP 目录**：模型目录放在节点能访问的内网 HTTP 文件服务器上，按
  [从 HTTP 目录注册模型](import-from-http.md#从-http-目录注册模型)登记，由节点自己下载。模型目录**没有**
  「管理节点中转」，HTTP 服务器必须是节点能直接访问的。
- **节点间同步**：已有一台节点有完整副本时，用[节点间同步](peer-sync-models.md)复制过去。

整个集群没有公网时，再按[完全离线：两条「禁止」规则](configure-pull-settings.md#完全离线两条禁止规则)加两条站点规则（Hugging Face 与 ModelScope 各一条，无替代地址、关闭回源），这样缺少来源的部署立即说明原因，不用等公网下载超时。

## 说明

server 不保存、也不接受镜像 tar 包上传；镜像字节只经内网镜像仓、HTTP 归档或节点间同步到达节点。
离线声明不等于 OS 级断网证明；要验证无公网访问，需额外施加网络隔离或抓包。
