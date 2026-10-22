# 离线环境部署

**适用场景**：集群（或部分节点）不能访问公网：拉不到 Docker Hub 等镜像仓，也下载不了 Hugging Face / ModelScope 上的模型。
本指南讲怎么把**镜像**和**模型**带进内网，让部署照常进行。

**前置条件**：

- 一台能上网的机器（下文叫「外网机」），用来下载镜像和模型，再用 U 盘、跳板机、内网文件服务器等任意方式拷进内网。
- 管理员权限。

## 先看结论：最省事的做法

| 要带进去的 | 首选 | 什么时候换别的 |
|---|---|---|
| server 与 agent | 发布包拷到 server 主机安装；节点用离线包 `gpuworks-agent.run` | — |
| 镜像 | 拷 `docker save` 归档到节点，`docker load`，平台自动认出 | 节点是 udocker（runveil）或不方便登录节点：[经管理节点中转](#方式二经管理节点中转从-url-导入)；节点多且长期离线：[内网镜像仓](#方式三内网镜像仓) |
| 模型 | 拷模型目录到节点，模型页「注册模型」→「计算节点本地」 | 节点多：放到 NFS 上[扫描导入](#方式二nfs-加扫描导入)，所有节点共用 |
| 第 2 台及以后的节点 | 不用再拷：同一同步组的节点在部署时自动从已有的节点复制 | 节点之间不通时，每台都按上面做一遍 |

整个集群都没有公网时，最后再加两条[「禁止」规则](#最后一步让缺来源的部署立刻报原因)，缺镜像或模型的部署会立即说明原因，而不是等公网超时。

## 第一步：在外网机上准备

### 确认要哪些镜像

部署用的镜像由推理模板决定。在镜像页「系统镜像」里按引擎和硬件找到对应版本，复制它的**镜像引用**，例如
`gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2`。手动指定节点部署时，部署页的逐节点预估也会列出要拉取的镜像。

在外网机上拉取并导出。注意节点的 CPU 架构（昇腾、GB10 等多为 arm64）：

```bash
REF=gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2
docker pull --platform linux/amd64 "$REF"      # arm64 节点改成 linux/arm64
docker save "$REF" -o runner-cuda13.0-sglang.tar
```

外网机在国内访问 Docker Hub 慢时，可以从 SWR 等镜像副本拉取，拉完后用 `docker tag` 打回上面的规范引用再 `docker save`：
平台按**规范引用**认镜像，归档里只有副本地址的 tag 时，节点上手动 `docker load` 之后平台认不出来。

### 下载模型

用官方 CLI 下载完整目录（含 `config.json`、tokenizer 与全部权重分片）：

```bash
# Hugging Face（国内可设 HF_ENDPOINT=https://hf-mirror.com）
huggingface-cli download Qwen/Qwen3-8B --local-dir Qwen3-8B
# ModelScope
modelscope download --model Qwen/Qwen3-8B --local_dir Qwen3-8B
```

用 `huggingface-cli download --local-dir` 下载的目录带有 `.cache/huggingface/download/*.metadata`，导入后模型能固定到对应的 commit，
建议原样保留这个隐藏目录。

## 第二步：安装 server 与节点

- **server**：把发布包拷到 server 主机安装，见[安装和升级控制面](install-server.md)；以后升级用
  [`upgrade --archive`](install-server.md#离线升级)，不发任何网络请求。
- **节点**：节点能连到 server 时，安装命令直接从 server 下载 agent，不需要公网。节点连 server 也不方便下载时，用
  [离线包](connect-nodes.md#离线包) `gpuworks-agent.run`：从 server 的 `/dl/gpuworks-agent.run` 取到后拷到节点执行。

server 内嵌了系统镜像目录和推理模板目录，离线也能直接用，不需要先更新目录。

## 第三步：导入镜像

### 方式一：在节点上直接 load（最快）

适用于 Docker 或 nerdctl 运行时的节点，需要能登录节点。

```bash
# Docker 运行时
docker load -i runner-cuda13.0-sglang.tar
# nerdctl 运行时（昇腾节点常见）：必须带 gpuworks 命名空间，否则平台看不到
nerdctl -n gpuworks load -i runner-cuda13.0-sglang.tar
```

agent 每 60 秒上报一次镜像目录里各镜像在本节点是否存在，load 完约一分钟后，镜像页的节点行变成「已存在」；不等上报直接部署也可以，
副本启动时节点会先查本地，有就直接用。前提是：

- 镜像已在镜像目录里且是启用状态（系统镜像默认就在；自己的镜像先在「自定义镜像」里添加）；
- load 后的 tag 与镜像引用**完全一致**（`docker images` 核对，不一致就 `docker tag` 补上）。

udocker（runveil）节点没有 `docker load` 可用，改用方式二。

### 方式二：经管理节点中转，从 URL 导入

适用于任何运行时，节点本身什么都访问不到也能用，全程在控制台里点。

1. 把归档放到 **server 能访问**的 HTTP 地址。最简单的是拷到 server 主机上，就地起一个临时文件服务：

   ```bash
   cd /data/offline-images && python3 -m http.server 8090
   ```

2. 镜像页打开该镜像，在节点表要导入的那一行，「获取方式」选「从 URL 导入…」，填
   `http://127.0.0.1:8090/runner-cuda13.0-sglang.tar` →「解析」，核对归档里的镜像引用 → 勾选「管理节点中转」→「拉取」。
3. server 边下载边推给节点，不落盘；完成后节点行显示「已存在」。

地址记进该镜像的「获取地址」后，其他节点的这一行也能直接选它。节点自己就能访问这个 HTTP 地址时，不勾「管理节点中转」，节点直接下载。
解析规则、压缩归档等细节见[从 HTTP 文件服务器导入](import-from-http.md#节点获取方式直连或管理节点中转)。

中转传输不可续传，中断后从头开始；几十 GB 的镜像、链路又不稳时，优先用方式一。

### 方式三：内网镜像仓

节点多、长期离线、镜像经常更新时，一次配置后部署就和在线一样自动拉取：

1. 把镜像同步进内网 registry（Harbor 等），保持原来的仓库路径，例如 `registry.corp.example/gpuworks/gpuworks-runner:<tag>`。
2. 设置 → 拉取设置 → 来源规则，加一条 `gpuworks/` → `registry.corp.example/gpuworks/`，并关闭「允许回源」，
   节点就不会再去公网拉。仓库要登录时配[凭据](configure-pull-settings.md#凭据)，规则写法见[来源规则](configure-pull-settings.md#来源规则)。

云上没有公网的实例可以用云厂商镜像服务的 VPC 地址，例如阿里云 ACR 的 `registry-vpc.<地域>.aliyuncs.com`。

### 分发到其他节点

一台节点有了镜像后，同一[同步组](configure-pull-settings.md#同步组)的其他节点在启动副本时自动从它复制（新装平台默认所有节点在一个组）。
也可以在镜像页节点行的「获取方式」里选「从其他节点同步…」，见[节点间同步](peer-sync-models.md)。这要求两台节点之间能直连。

## 第四步：导入模型

### 方式一：拷到节点后登记（最快）

1. 把模型目录拷到节点上，例如 `/opt/gpuworks/models/Qwen/Qwen3-8B`（默认本地存储的目录；放在别的位置也可以）。
2. 模型页「注册模型」→ 来源选「计算节点本地」→ 选节点、填目录的绝对路径 → 预览后注册。平台读取 `config.json`
   补齐架构、精度、上下文长度等元数据。

想让部署时自动命中推理模板的参数预设，可以换一种登记方式：先按 Hugging Face / ModelScope 的仓库名登记模型（只读元数据，不下载，
需要 server 能访问这些站点；不能访问时用上面的「计算节点本地」），再在模型详情里对该节点「登记路径」，见
[离线导入模型](configure-pull-settings.md#离线导入模型)。

登记只记录位置，不复制也不移动文件；目录里有 `.part` 等未完成下载的文件时登记会失败。

### 方式二：NFS 加扫描导入

模型多、节点多时，把模型按 `<组织>/<仓库名>/` 放到 NFS 上：

1. 「存储」页「添加存储」，类型选「NFS」，填导出地址并「试挂」。
2. 存储卡片上点「扫描导入」，勾选要的模型「导入所选」。

所有节点挂载同一份，不用逐台拷贝。详见[导入已有的模型库](storages.md#导入已有的模型库)。

### 其他方式

- **内网 HTTP 目录**：模型目录放在节点能访问的内网文件服务器上（autoindex 列表页），模型页「注册模型」→「HTTP 目录」，
  由节点自己下载，见[从 HTTP 目录注册模型](import-from-http.md#从-http-目录注册模型)。模型**没有**管理节点中转，HTTP 服务器必须是节点能直接访问的。
- **从浏览器上传**：存储详情里可以把文件上传到存储的 `files/` 目录（可选上传后解压 tar/zip），然后按方式一登记该目录。
  上传不支持续传，只适合小模型，见[浏览存储里的文件](storages.md#浏览存储里的文件)。

### 分发到其他节点

与镜像相同：同一同步组的节点部署时自动从已有完整副本的节点复制，也可以在模型页手动同步，见[节点间同步](peer-sync-models.md)。

## 第五步：部署并验证

照常部署，见[部署模型](deploy-models.md)。节点本地已有的镜像和模型直接使用，不触发下载。部署后在实例详情确认没有经过拉取阶段，
再用试验场（Playground）或 `/v1/chat/completions` 验证。

## 最后一步：让缺来源的部署立刻报原因

整个集群都没有公网时，按[完全离线：两条「禁止」规则](configure-pull-settings.md#完全离线两条禁止规则)给 Hugging Face 与 ModelScope
各加一条站点规则（无替代地址、关闭回源）。之后漏导入模型的部署会立即给出 `weights_fetch_forbidden` 原因，不用等公网下载超时。
镜像对应的做法是方式三里关闭「允许回源」的来源规则。

## 节点不能出公网

常见于云上没有公网 IP、也没有 NAT 网关的实例（例如阿里云 VPC 里的 ECS）：agent 能连上 server，但节点拉不到镜像、下载不了模型。
上面的方法都适用，按节点能访问什么来选：

| 节点能访问 | 镜像 | 模型 |
|---|---|---|
| 只有 server | [管理节点中转](#方式二经管理节点中转从-url-导入) | 拷到节点后登记（经 Web 终端、scp 等能到达节点的方式） |
| 内网 / 同 VPC 服务 | [内网镜像仓](#方式三内网镜像仓)（阿里云 ACR VPC 地址） | 同 VPC 的 NAS 加[扫描导入](#方式二nfs-加扫描导入)；或 `ossutil` 经 OSS 内网地址 `oss-<地域>-internal.aliyuncs.com` 下载后登记（`ossutil` 安装包本身要先拷到节点） |
| 已有该镜像/模型的另一台节点 | 节点间同步 | 节点间同步 |

agent 怎样接入不通公网的节点，见[节点与 server 网络不通](connect-nodes.md#节点与-server-网络不通)。

## 更新目录（可选）

想用比内嵌更新的系统镜像目录与推理模板目录时：在外网机上从任一发布源（如 `https://dl.gpuworks.ai`）下载 `manifest.json`
与 `<channel>/index.json` 里列出的 `catalog/templates/<sha256>.tar.gz`，在设置页「版本与更新」点「上传 manifest」「上传目录包」
（等同 `POST /api/v1/images/import` 与 `POST /api/v1/inference-templates/official`），规则见[镜像目录](../reference/image-families.md#更新目录)。
离线环境里「检查更新」会因为发布源不可达返回 502 `UPDATE_SOURCES_UNAVAILABLE`，不改库。

## 说明

- server 不保存、也不接受镜像 tar 包上传；镜像字节只经节点本地 load、HTTP 归档（含管理节点中转，不落盘）、内网镜像仓或节点间同步到达节点。
- 离线声明不等于 OS 级断网证明；要验证无公网访问，需额外施加网络隔离或抓包。
