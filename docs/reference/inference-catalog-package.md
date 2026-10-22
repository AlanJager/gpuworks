# 可执行推理目录包

目录包是内容寻址的多文件包，数据库只保存 digest 和投影。根 manifest 描述包版本；每个模型
位于 models/<model-id>/model.yaml，可携带该模型的轻量 assets/。镜像、权重、框架库、venv 和
共享运行时不能打包进 assets，必须通过镜像、权重源或节点资产交付。

    manifest.yaml
    models/<model-id>/model.yaml
    models/<model-id>/assets/<small-file>

model.yaml 以 ADR 0009 的扁平 spec 为核心：模型元数据、spec 列表、GPU 谓词、engine、image、
weights、args、env、pre_start 和 API contract。spec 不写 tp/nodes；部署时由实际分配 GPU 推导
并行度。pre_start_argv 与 pre_start_script 二选一，脚本上限 64 KiB。

server 对包执行路径安全校验、schema 校验和内容 digest；重复 digest 可复用，损坏缓存不能以旧
digest 提供服务。导入、导出和同步接口见 API 参考与 ADR 0009、0010。

spec 的 args 里取值为文件的插件类参数（`--reasoning-parser-plugin`、`--tool-call-parser-plugin`
等 `--*-plugin`）必须能由 spec 自身提供来源：绝对路径，或 assets、pre_start 脚本、component 挂载
给出的路径。相对取值按容器工作目录解析，而该目录固定为 `/`，因此裸文件名无法解析——曾经有 spec
声明 `--reasoning-parser-plugin nano_v3_reasoning_parser.py`，该文件不在镜像、权重仓库和 assets 中，
引擎启动即 `FileNotFoundError`。此时校验报稳定错误码 `CATALOG_FILE_VALUED_ARG_UNRESOLVED`。
镜像内容不在控制面手里，所以该检查只拦"spec 内完全无来源"这一可静态判定的情形；文件是否真的在
镜像里由 L2/L3 探测回答。

## 导入：API、打包卫生与覆盖语义

导入接口是 `POST /api/v1/inference-templates/import?overwrite=true`，请求体为**原始字节**
（非 multipart），服务端上限 32 MiB。按 gzip magic 分流：gzip 输入按目录包处理——assets
与 README 只能经这条路径进来；非 gzip 输入按裸目录 YAML 解析，此时引用本地文件（assets、
文档）会报稳定错误码 `CATALOG_PACKAGE_REQUIRED_FOR_FILES`。成功响应 `{models, package_digest}`。
Web UI 的「推理模板 → 导入模板」调用这个接口（官方目录包走设置页「版本与更新」的上传，见 `POST /api/v1/inference-templates/official`），
文件选择框接受 `.yaml`/`.yml`/`.json` 与 `.tar.gz`/`.tgz` 目录包。

tar 包有三条卫生要求，每条都对应一个真实 400：

- 条目名必须是 clean 相对路径：`tar -C pkg .` 会产生 `./` 前缀，报
  `unsafe inference catalog package path "./manifest.yaml"`。目录条目本身会被跳过，但
  软链等非 regular 条目报 `is not a regular file`；
- macOS AppleDouble 文件（`._x`）报 `contains unexpected file`，打包前置
  `COPYFILE_DISABLE=1`；
- 从包目录内用 `-T` 显式列文件最稳：
  `COPYFILE_DISABLE=1 tar -czf pkg.tgz --no-recursion -T <(find . -type f ! -name '._*' | sed 's|^\./||' | sort)`。
  注意 zsh 不对裸变量做单词拆分，`tar ... $FILES` 会把多行列表当成一个参数——用 `-T`，
  不要用变量。

覆盖语义是**棘轮**：导入只新增或按名更新 model/spec，从不删除 server 上已有的 user 行
（不在新包里的旧 spec 原样保留，仍然可见、可选、可部署）。下线 user spec 要显式删除：
`DELETE /api/v1/inference-templates/spec?model_id=…&spec=…`（模板详情页的删除按钮），仍有部署
引用它时拒绝。发布方应把"包里删 spec"当作一次需要附带这一步清理的变更来对待。

assets 的交付路径：包体（含资产字节）按内容寻址存于包仓库，部署时 server 把资产内容
（带 sha256）作为 RuntimeAsset 写进实例 DesiredState 下发，agent 校验后解包到只读缓存再
挂到 `unpack_to`——节点不需要访问任何外部地址。编写资产时**优先打成单个 .tar.gz 归档**
（`unpack_to` 指向目录）：旧的 udocker CLI 启动器对单文件 bind mount 会**静默失效**（目录挂载
才生效），裸文件资产在这类节点上表现为"部署成功但文件不在"，排查成本远高于多打一层包。runveil
（`udocker` driver 的默认启动器）直接以 PRoot 绑定挂载，但未就单文件资产单独验证，仍按目录分发。

## model_config：模型级的估算事实

每个模型（不是每个 spec）可以声明一个 `model_config`，内容是**该模型 config.json 中与显存
估算相关的子集**，字段名就是 config.json 自己的字段名：

    model_config: {model_type: qwen3, num_hidden_layers: 64, hidden_size: 5120,
      num_attention_heads: 64, num_key_value_heads: 8, head_dim: 128, max_position_embeddings: 40960}

为什么放在目录里：目录模板部署时不会注册模型，因此没有本地 config.json 可读；受限（gated）
仓库服务端根本取不到，部分仓库的 config.json 不写注意力形状（Gemma 3），扩散类模型甚至没有
config.json。"KV pool 能装多少 token" 只能由目录自己回答，否则部署页只能显示"配置不可用"。

`kv_cache: false` 是唯一一个不是 config.json 字段的值，用于声明该模型**没有** token 索引的
KV cache（扩散/纯编码器类）。此时估算给出的是"不适用"这一确定结论，而不是"配置不可用"。
ModelScope 与 HuggingFace 使用同一份 config.json 布局，所以该字段与权重来源无关。

维护工具是 `cmd/gpuworks-catalog-model-config`：它从 spec 已声明的权重源读取真实 config.json
（受限仓库需要 `HF_TOKEN`），写入解析后的形状；`-verify` 再用已发布的权重索引核对层数与注意力
类型（MLA 潜在量 vs 显式 K/V）。少数仓库读不到形状时，`fallback-sources.json` 记录所用的镜像
仓库、子目录或逐字段声明的形状及其依据。`internal/server/inferencecatalog/seed` 的测试遍历内置
目录，要求每个可部署模型要么能算出每 token 字节数，要么显式声明无 KV cache。

已知的两处良性差异：权重索引可能多出一个 MTP 层（DeepSeek-V3 系的 61+1）或含多个并行堆栈
（视觉/音频编码器），`-verify` 已按此归类；`dots-studio/dots3-note-prev` 的 checkpoint 比
config 多一层，服务引擎按 config 建模型，因此以 config 为准。
