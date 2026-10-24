# TODO — 待决策项

本文件记录需要决策的问题、原因和修复建议；实施进度放 `plan.md`，已确认但决定暂不修的缺陷放 `known-issues.md`。已解决的事项按月归档到 `artifacts/archive/todo-resolved/<YYYY-MM>.md`（2026-09-17 迁出 T1、T3、T4、T9、T11，2026-09-25 迁出 T16，2026-09-27 迁出 T8、T17、T19，见 `artifacts/archive/todo-resolved/2026-09.md`；2026-10-01 迁出复核时已修的 T12、T13、T15 与当日修复的 T5、T26、T33、T38 及 T36 的环境变量部分，2026-10-02 迁出 T39 的批量查询部分、T23 与 T29 的 PPU 分区用例、L2 输出、`TestLanes_DataLaneDrop_CtrlSurvives` 三部分，见 `artifacts/archive/todo-resolved/2026-10.md`）。2026-10-02 将 T20、T41 迁入 `known-issues.md`（已确认、决定暂不修）。同日迁出 T35 的失败原因部分，并将 T40 迁入 `known-issues.md`。2026-10-03 将 T53 迁入 `known-issues.md`。同日迁出 T36（实例日志部分）与 T52，T52 余下的页面记为 T55；同日迁出 T55 中与共享存储改造不重叠的部分及当日记录并修复的 T57（server_dial 节点自升级），2026-10-04 迁出当日记录并修复的 T58（CompShare 规格矩阵两条用例），并迁出 T7（副本放置已实现）与 T10（引擎字典快照已收窄）、将 T45 迁入 `known-issues.md`（等旧事件行随保留期淘汰）。2026-10-09 迁出 T56（请求途中数据通道断开改报可重试的 503）。2026-10-12 迁出 T63（克隆模板后编辑器加载了源模板的 YAML）。2026-10-17 将原 T54（调度不计入卡上非本平台进程占用的显存）迁入 `known-issues.md`，编号改为 T68（T54 在那里已被另一项占用）。2026-10-17 同日将 T51（两个持续很短的读写竞态）迁入 `known-issues.md`。同日迁出 T70（runveil 导入老 Docker 归档时不认符号链接与硬链接的层）。2026-10-19 迁出 T73 的目录补全部分（195 个模型的配方补上 API 声明），余下 `pfnet/plamo-2-translate` 与部署前提示仍在本文件。2026-10-20 迁出 T74（Vast 首次安装等平台公钥生效）与 T76（来源规则端点超时，原因是测试环境自加的规则未限定节点，不是产品缺陷），并将 T75（模型下载跑不满带宽）迁入 `known-issues.md`。同日迁出 T73 余下部分（`plamo-2-translate` 改声明新增的补全接口 `openai.completions`，部署表单提示模型没有声明接口）。同日迁出 T77（关闭 SSH 主机密钥校验）与 T81（阿里云安装等云助手上线），并将 T80（拉镜像失败消息里错误行排在进度条之后）作为体验问题迁入 `known-issues.md`。同日迁出 T101（nerdctl 拉取大镜像在解包阶段被误判无进度；`feat/k8s-nodes` 分支记录时编号为 T73，与已归档的 T73 重号；T82 已被并行分支用于 PRoot 公平调度，故跳号）。2026-10-21 将 T102、T103 迁入 `known-issues.md`（只影响关闭 native 拉取的 udocker CLI 路径）。
本次复核：2026-10-01（前次 2026-09-30，迁出 T24 多机启动屏障、T27 引擎端口、T30 昇腾多机参数网络）。T6 为 2026-09-15「启动配置单一事实源」评审拆出；T14 为 2026-09-17 KV 磁盘卸载工作中发现；T21 为 2026-09-22 KV offload 调研的产品化后续；T28 为 2026-09-29 PD 分离的真机核实所记（T24、T25 同日验证后迁出）；T29 为同日部署收敛的完整回归中所见；T31、T32 为 2026-09-30 加速器参数网络实现后留下的缺口与待决策项（T30 同日解决后迁出）。T34–T37 为 2026-10-01 ninfer 在 5090 容器实例上的试用所见（`artifacts/research/2026-10-01-ninfer-5090-trial/README.md`）。T39、T42 为同日修复 T26、T33、T36、T38 时发现的后续。T43–T48 为同日 store 列清理（`artifacts/archive/2026-10-01-store-typed-columns/plan.md`）按 ADR 0032 留下的待决策项。T49 为 2026-09-30 实例级生命周期 A5 真机验证所记，T51 为 2026-10-02 MR !238 评审所记（同日副本放置计划讨论所记的 T50 于 2026-10-03 实现后迁出）。T55 为 2026-10-03 修 T52（按 1000 节点 / 3000 实例复核副本放置时所记）时留下。T57 为同日调整添加节点表单时所记，当日修复后迁出。T59 为 2026-10-08 编写单机 4×RTX PRO 6000 TensorFold 推理模板时所记；T60 为同日镜像目录收窄时所记；T62 为同日讨论 T59 方案时核对 agent 回收逻辑所见。

| 问题 | 影响 | 建议顺序 |
| T2：目录镜像未固定内容 | 验证结果难以复现，节点缓存可能不同 | 随后固定已验证镜像 |
| T6：硬件适配三层混在一起，且发生在估算之后 | 同一个"Turing 上 bf16"按通道不同有三种结局；昇腾无模板部署缺 `--device npu` | 在「启动配置单一事实源」落地后做；先只要求 dtype 类 key 允许 `auto` |
| T14：docker 运行时的镜像拉取代理与超时 | `image_pull_proxy` 对 docker 无效、节点同步拉取无代理/改写选项、30 GB 镜像拉不完 30 分钟超时 | 代理经 dockerd 的 systemd 环境或 `--config` 传入；同步路径接 pullref 候选；超时按进度而非总时长 |
| T18：完整启动信息透明化 | 镜像差异表、解析后方案与节点实际执行信息混用，无法完整核对启动行为 | 独立设计，复用 ResolvedLaunch，补齐只读节点事实 |
| T21：KV offload 产品化 | 当前 KV 磁盘卸载只按"有没有盘"开启，调研证明磁盘层扛不住热流量、真正有效的是按 KV 压力开启的 CPU 内存层；用户无依据配置 offload | 在已有 KV 磁盘卸载（`plan-9-16-kv-disk-offload`）之上补 CPU 层与压力驱动，先做建议与告警，再做自动开启 |
| T28：昇腾 PD 的 NPU 网络拓扑平台不知道；主机 RoCE + Mooncake KV 池路线是否产品化 | 选了 NPU 网络上不互通的两张卡（例如非同号卡）时交接必然失败；KV 池路线能绕开 NPU 网络但依赖引擎补丁与附属进程 | 由 agent 上报 NPU 口的 LLDP 对端，放置时校验互通；KV 池路线待决策 |
| T31：昇腾多机与跨机 PD 选错卡对会启动后才失败 | 卡连通性测试能告诉运维哪些卡互通，但调度不看它；普通多机部署也不能逐 rank 钉卡，只能用 `placement.gpus` 让各节点钉同一卡号 | 把最近一次卡测试的结果接进放置校验；或先允许逐 rank 钉卡 |
| T32：卡连通性测试结果的展示方式待定 | 只有一个节点对时直接显示卡矩阵、跳过了节点对那一层；"仅同号卡"单独一档（蓝色、算通过）而不是并入"部分通" | 待决策：是否统一成"先节点对（全通绿 / 部分通黄 / 全不通红）、点进去看卡" |
| T34：节点间镜像同步由源节点直连目标节点内网地址 | 内网节点与公有云节点都连得上 server 却互不相通，同步必然失败（`dial … no route to host`）；部署表单仍默认推荐这条路径 | 目标不可直连时经 server 数据通道中转；表单按可达性推荐 |
| T35：获取失败的部署没有重试入口 | 失败原因已显示，但重新分发镜像只能去镜像页手工操作，URL 来源还要自己找回来 | 在部署的失败说明旁给出重试入口，带上原来的 URL 来源 |
| T37：模型目录格式在下载完成后才校验，引擎自带格式不被接受 | 22 GiB 下完才判“不是模型”；单文件 `.ninfer` 等格式只能放桩文件绕过 | 登记/下载前按文件清单校验；为引擎自带格式提供显式类型 |
| T39：运行中实例存活检查的后续 | 引擎进程还在但不健康时不处理；nerdctl 的运行中容器列表未经真机确认 | 不健康的处理待决策；在昇腾 nerdctl 节点核对一次列表与 inspect 一致 |
| T42：云实例列表完整性有两处未经真机确认 | 同步会删掉列表里没有的实例行；列表若不完整会误删行并停掉节点的带外恢复 | 用跨区域、超过 100 台的账号核对 CompShare；核对 RunPod、Vast 是否分页 |
| T43：`instances.schedule_expl` 承载启动事实 | 参数网络、PD 端点、unit 归属这些后续决策依赖的事实藏在放置解释的 JSON 里，迁移曾用 `json_set` 改其内部字段 | 待决策：是否把启动事实提成列，解释部分留作快照 |
| T44：`deployments.node_ids` 是关系却存成 JSON | 驱逐检查、调度与 DTO 都要按节点反查部署，只能先读出部署行再在 Go 里解码 | 待决策：是否改为 `deployment_nodes` 关联表；解码已收敛到 `Deployment.StoredNodeIDs` |
| T46：`deployments` 上违反 ADR 0032 的三列 | `env` 有读者无写入方；`placement`、`container_overrides` 存的是请求原文而非归属类型序列化的值，未知键与两种拼写都会被持久化 | 待决策：`env` 删列还是恢复写入；另两列在写入口收敛到归属类型 |
| T47：store 里其余疑似违反 ADR 0032 的列 | benchmark 的两列没有具名归属类型、一列存请求原文；`registry_sources` 一列装两类条目 | 逐项核实后给出归属类型、分开存，或写明维持现状的理由 |
| T48：`instance_events.data` 不满足 ADR 0032 第 2 条 | 新行存事件 code 的参数 `map[string]any`（通用口袋，无具名归属类型），旧行是自由文本，一列两形态 | 旧行随 retention 淘汰后（与 T45 同时）只剩参数形态；待决策：按 code 给参数具名类型，或写明维持 `map[string]any` 的理由 |
| T49：混用 GPU 架构的 PD 各实例 attention backend 不一致，NIXL 交接必然失败 | 同一部署里 Ada 与 Turing 混用时每个请求都 KV load 失败（`kv_load_failure=fail` 下 500），预检只给 `PD_GPU_MODELS_DIFFER` 警告 | 由平台为 PD 部署统一 attention backend（取各实例都支持的交集）或在评估时报错；决定前在文档里写明需手动加 `--attention-backend` |
| T55：模型页与存储页仍取全量节点列表 | 两页每 15 秒拉全部节点，1000 台节点时每次数 MB；模型页还把全体节点逐个写进 AI 助手的提示词 | 等共享存储改造（`feat/nfs-1`，重写这两页）合入后改：节点选择用 `NodePicker`，存储页的节点行与用量合并成分页接口 |
| T59：推理模板表达不了 KV 磁盘层与托管可写目录 | vLLM 磁盘层、SGLang HiCache、callable 引擎的落盘缓存都只能在部署表单开或在模板里写成不透明参数；后者写进容器可写层，没有宿主挂载、配额与清理，调度也不知道 | 按 `artifacts/work/2026-10-08-template-cache-dir/plan.md`：先做 shm 由 CPU 层推导、spec 声明 `cache`（路径与上限）并按「剩余 ≥ 镜像 + 声明 + 磁盘保留空间」准入、vLLM 磁盘层参数解码；挂载、用量统计与主机内存准入在后 |
| T60：镜像目录里只剩写入、界面已不再展示的列 | `image_versions.verification_state/verification_error/verified_at`、`rolling_tag`、`upstream_gone`、版本级 `framework`，`image_series.common_args/common_env/deprecated_since`：2026-10-08 镜像目录收窄（`artifacts/work/2026-10-08-image-catalog-simplify/plan-v2.md`）后界面不再读它们，但 manifest 导入与 store 仍在写，留着会让人以为还有用。`image_series.purpose` 不在此列：表单与列表已不展示，但部署页仍按 `purpose === "inference"` 过滤系列，后端继续默认 `inference` | 随下一次 store 列清理一起删（迁移 + 旧数据测试），删前确认部署页只读展示的 `common_args` 已归推理模板；`rolling_tag` 若届时仍被滚动 tag 重拉与自动选镜像排除读取，则保留 |
| T64：厂商 CLI 持续不应答时，已知卡的健康会被无限期冻结 | 只在卡**第一次**被看到时写入完整结论；之后 `adoptPresence` 永远保留上一次 CLI 的答案。一张先前读到 OK 的卡，在 CLI 永久失效后仍然一直以「正常、可调度」被下发，只有节点级的「厂商工具未响应」提示；真机上 172.30.8.32 的两张卡则冻结在旧 agent 写的「Error 且无 finding」，运维看到故障却没有任何可处理的线索 | 待决策：区分「偶发一次超时」与「持续问不到」。建议按 `gpu_probe_failed_at` 的持续时长分档（它自 MR !404 评审起记的是这一轮连续失败的起点，正好量得出时长）：短时保留旧结论（现行行为），超过阈值后改写为 `vendor_cli_unanswered`；另外存储结论里没有 finding code 的（旧 agent 只报布尔值）不值得保留，直接采纳本轮的 |
| T65：NVIDIA 其实不需要厂商 CLI，但平台把它当成唯一来源 | CLI 哑了就连显存、UUID、健康全部拿不到，逐卡 CUDA 冒烟还因为「没有 UUID」一并跳过 —— 恰好在最需要独立健康信号时自己也失效。实测两条独立来源都可行（gpu3，2026-10-15）：`/proc/driver/nvidia/gpus/<BDF>/information` 给 **GPU UUID + Device Minor**（容器注入要的号）+ 型号；libcuda 经 ctypes 给 **型号、显存、UUID、PCI 地址**，且 `cuCtxCreate`+`cuMemAlloc` 本身就是比读计数器更强的存活证明。两者 UUID 与 nvidia-smi 完全一致 | 新增「无 CLI 降级探测」：冒烟的 UUID 改从 procfs 取（断开「CLI 哑→冒烟也跳过」的循环），显存/身份在 CLI 缺席时由 libcuda 补。仅 NVIDIA 可行 —— 昇腾 .31 实测坏在 dcmi 管理面本身（`dcmi module initialize failed`、dmesg `Devdrv_get_pcie_id_info failed`），任何 libdcmi 退路同样失效，且 `/dev/davinci0..7` 有 8 个节点而本机只有 4 张卡（其余已重绑 vfio），按号注入会把别人虚拟机的卡塞进容器。所以 T64 的阈值方案只对没有退路的厂商保留 |
| T69：镜像 e2e 夹具起的私有 dockerd 会删掉宿主机的 docker0 | `test/e2e/image_runtime_fixture.py` 以 `--bridge=none` 启动 dockerd，只隔离挂载命名空间、与宿主机共用网络命名空间；moby 在 `--bridge=none` 时会删除名为 docker0 的网卡，所以在装了系统 docker 的机器上手动跑一次 `make test-e2e-image-*`，系统 docker 的所有 bridge 容器就会断网、新容器起不来（与 2026-10-17 ppu1 上外部私有 dockerd 删 docker0 同一机制）。不在 CI 中 | 让 dockerd 进独立网络命名空间；但 registry-pull / registry-mirror 要 dockerd 拉宿主机 `127.0.0.1` 上的测试仓库，需在命名空间内同端口起转发、经共享文件系统上的 unix socket 接回宿主机。改完在本机跑通 image-runtime 与两个 registry 用例 |
| T71：bench 的 tokenizer 由 agent 直接从 huggingface.co 取，不走代理与来源规则 | `pkg/bench/tokenizer/ref.go` 用 `http.DefaultClient` 直连 huggingface.co；节点配了拉取代理、站点规则（镜像站、禁止回源）或凭据时仍被忽略，离线或大陆节点上评测的 tokenizer 取不到 | 取 tokenizer 改走节点的拉取代理与站点规则候选（复用 `modelfetch` 的候选与冷却），gated 仓库带站点凭据；或评测时优先读模型目录里已有的 tokenizer 文件 |
| T72：ModelScope 模型 logo 是任意 CDN 地址，不在来源规则与代理的约束内 | logo 地址来自 ModelScope 返回的元数据，管理节点取 logo 经「用于管理节点出站访问」的代理，但不套站点规则；管理节点连不上该 CDN 时静默没有 logo，用户看不到原因 | 要么允许 logo 域名也匹配站点规则，要么不可达时在模型卡上提示「logo 不可用」；先确认是否值得投入 |
| T78：runveil 下 force 删除部署不回收容器 rootfs | 2026-10-20 Vast 卡塔尔节点上三个被 force 删除的部署各留下约 33 GB 的 `/opt/gpuworks/udocker/containers/gpuworks-<实例>`，盘满后新部署报 `clone cached base rootfs: exit status 1`，错误里看不出是磁盘问题 | agent 在实例从期望状态消失时删除其 runveil 容器目录（与 docker 路径的孤儿清理对齐）；`clone cached base rootfs` 失败时带上底层错误（如 ENOSPC） |
| T79：主机驱动低于镜像所需 CUDA 版本时只报 `No accelerator` | 2026-10-20 主机驱动 550、镜像 CUDA 13（需 ≥580），引擎启动后才报 `RuntimeError: No accelerator`，平台不提示驱动过旧，也不提示镜像自带的 forward-compat 库可用 | 部署前用节点驱动版本与镜像 `NVIDIA_REQUIRE_CUDA` 比对给出提示；runveil 下若镜像带 `cuda-*/compat` 且版本足够，可作为显式选项前置 |
| T82：PRoot 事件循环在上千线程下饿死旧线程，公平调度补丁在真机上让权重加载卡住 | SGLang TP8 在 runveil 下约 3100 个线程时旧线程长时间 tracing-stop；目前只靠限制 SGLang 线程池缓解，补丁未内嵌 | 按 `artifacts/work/2026-10-20-runveil-many-threads/plan.md` 的区分办法逐项隔离（先只留哈希表、再限批），真机验证后再决定是否重做 |
| T83：内嵌 PRoot 没有 `proot-temp-noise.patch` 的效果，容器失败时 PRoot 的无害报错盖住引擎真正的错误 | 2026-10-20 本地用 `internal/assets/bin/proot-x86_64-static-nofd` 复现：以 root 在 `/root`（700）下降权运行、绑定到镜像里不存在的路径时，退出前打出 `can't chdir to '/root'`（退出码 0）；该补丁本应让它静默退回 `/`。Vast 上这句成了 stderr 最后一行，部署 26 的失败原因显示成它，真正的 `No accelerator` 被盖住 | 核对内嵌二进制的构建来源，用修好路径的 `build.sh` 重建并过门禁；agent 报容器失败原因时跳过 `proot error:`/`proot warning:` 行，取引擎自己的最后一条错误 |
| T100：经来源规则拉取的镜像，从节点删除后仍留着替代地址的 tag 与内容 | nerdctl、docker、udocker（native）节点上都要手工删，大镜像占着磁盘 | 删除时一并删掉平台经来源规则留下的替代地址 tag |
| T105：runveil 非交互 Exec 未恢复工作负载环境与挂载 | 容器已健康，但 Exec 内导入 torch_npu 找不到宿主驱动库；ExecPTY 正常 | 让 Exec 复用持久化 workload spec 并验证环境、挂载和设备选择 |
| T106：CANN 8.5 runveil 的 DCMI 初始化阻塞 | 镜像缺少容器标记和 systemd-detect-virt，DCMI 失败子进程在 CANN dump 析构中等待 | 修正容器检测阻塞并定位后续 TsdOpen 错误，补默认路径 chat |

## T2. 目录镜像标签会漂移

**状态**：发现于 2026-09-14 的 A100 复盘，未决定目标镜像版本。

### 原因与范围

模型权重 revision 已固定，但镜像按标签引用。相同 spec 在不同时间或节点缓存中，
可能对应不同的引擎、依赖和 CLI 行为，因而 A100 的 PASS 记录不足以保证后续部署可复现。

本次统计内置目录的 930 个 spec 镜像引用：

| 浮动标签 | spec 数 |
|---|---:|
| `vllm/vllm-openai:latest` | 402 |
| `lmsysorg/sglang:latest` | 175 |
| `eugr/spark-vllm-b12x:latest` | 3 |
| `quay.io/ascend/sglang:main-cann9.0.0-910b` | 1 |

原记录的 577 项准确，但只覆盖前两种镜像；上述显式浮动标签共 581 项。
2026-10-10 起模板改用 `gpuworks/gpuworks-runner` 带 revision 的 tag，按约定不再原地覆盖。1102 个 spec 里还剩 4 个浮动标签：`eugr/spark-vllm-b12x:latest` ×3、`lmsysorg/sglang:dev` ×1。
换镜像本身没有做真机回归，详见 `artifacts/work/2026-10-10-template-runner-images/plan.md`。下文的 digest 方案仍然适用。
目录中没有 digest 引用。其余明确版本标签也不能自动视为不可变：
[Docker 官方说明](https://docs.docker.com/build/policies/validate-images/)指出标签可以更新，digest 才固定内容。

### 推荐方案

**先固定已经验证过的版本，再支持并推广 digest；用户默认选模型即可部署。**

1. 从实际 PASS 环境提取镜像引用、registry manifest digest、平台架构、完整命令和权重 revision，
   复用已有部署快照与验收记录保存证据。不能把本地 image ID、离线 tar 的 SHA 或目录包 digest
   当作 registry manifest digest，也不能根据当天的 `latest` 推断过去验证用了什么内容。
2. 第一批只替换有证据的 spec，使用明确版本标签；其余保持待验证，
   不把全部模型统一改到某个未经验证的“最新稳定版”。这一步减少漂移，不承诺完全不可变。
3. 完整方案采用 `image: repo:version@sha256:...`，尽量复用现有字段。
   这不是纯目录数据改动：[`ValidateTagged`](internal/server/imagecatalog/manifest/manifest.go)
   目前拒绝 digest，[`pullref.Candidates`](internal/server/imagecatalog/pullref/pullref.go)
   也过滤 digest 来源，且现有测试明确固化了该行为。需先打通校验、候选、缓存和各运行时拉取链路。
4. 镜像来源必须仍指向固定内容；镜像站不提供相同 digest 时清楚报告不可用，
   不能静默改拉浮动标签。确认多架构 index 与目标平台 manifest 的对应关系，验证离线导入与 peer 缓存。
5. UI 显示容易理解的版本号，digest 放详情并支持复制。更新通过目录包发布，
   已有部署保留原快照，用户明确选择后再重建。

### 验收条件

- 引用校验与候选测试覆盖 tag、tag+digest、非法 digest、镜像站和多架构场景；
  Docker、nerdctl、udocker 验证实际获取及缓存的内容符合固定引用。
- 目录发布检查先阻止新增浮动标签，存量逐批减少；完全支持 digest 后再收紧发布要求。
- 每批真实回归后再更新验证结论，同时保留旧目录包供回滚。

实施 T2、T3 的生产改动时，按 `CLAUDE.md` 跑完整 Makefile 检查、相关 race/vet，
以及部署与运行时相关的 L2/L3；真实镜像/GPU 验证单独记录，不能把 skip 当成通过。

## T6. 硬件适配的归属：探测事实修正、运行时镜像要求、目录档位三层混在一起

**状态**：2026-09-15 评审提出，未实施；依赖「启动配置单一事实源」改造先落地（类型化配置成为估算与 argv 的唯一输入）。前置依赖「启动配置单一事实源」已于 2026-09-16 落地，可开工。

### 现状

- 目录里硬件身份是 `GPUSelector` 三元组（vendor、cc 范围、卡名正则），962 个档位的差异落在六个维度：
  镜像、env、args、pre-start、拓扑 resolver、权重量化。Turing 37 档全部换 cuda12.9 runner 镜像并加 `--dtype half`；
  昇腾 82 档全部带 resolver、59 档带 `--device npu --attention-backend ascend`；海光 42 档全部带 resolver、24 档要 pre-start 脚本；
  Blackwell 50 档带 `VLLM_USE_FLASHINFER_MOE_FP4` 类 env。
- 代码里的适配只有 [`quirks.go`](internal/server/deploy/quirks.go) 三条（Turing 无 bf16、DeepSeek MLA fp8 KV、MiniMax block size），
  作用在 argv、发生在估算之后；[`dtypematrix.go`](internal/server/estimate/dtypematrix.go) 的能力表只用于调度过滤。
- 同一件事三种通道三种结局：表单选 bf16 on Turing → 调度器 dtype 过滤剔掉节点；extra_args 写 bf16 → 估算看不见、quirk 不覆盖用户、引擎启动崩；
  表单留 auto → quirk 悄悄钉 fp16。
- 无模板部署（直接选模型 + 节点）拿不到运行时镜像要求的 flag：昇腾上没有 `--device npu`。
  `ImageSeries.CommonArgs/CommonEnv` 字段存在但 seed 未填，前端只把它当"常用参数按钮"往 extra_args 里追加。
- 自动模式节点在提交后才定，多机各 rank 硬件可能不同，因此适配不能在创建时算死。

### 建议

1. 三层归属。A 层探测事实修正在代码：只收从探测硬件事实可推导、与镜像无关的规则（今天只有 cc < 8.0 无 bf16），
   作用于类型化配置、发生在估算之前。B 层运行时镜像要求作为镜像系列数据（`CommonArgs/CommonEnv`）：`--device npu`、
   `--attention-backend ascend`、HCCL/GLOO IFNAME、VLLM_HCU_*、B12X_*，同一芯片换镜像就变，所以跟镜像走。C 层模型×硬件的选择留在目录档位。
   三层经同一个物化入口进入部署记录：字典内 flag 进字段，其余进残余 args 与 env layers。
2. 硬件相关 key（dtype、kv_cache_dtype）持久化 `auto`；`Adapt(config, 节点硬件事实)` 为纯函数，planning 对每个候选节点各算一次、
   launch 再算一次，永不落盘。显式填了硬件跑不了的值拒绝并给原因，不改写；`auto` 才由 Adapt 解析。
   `gpu_memory_utilization` 的按卡默认值已经撞上这一条：它一旦进了表单就成为具体值、换节点不再重算，
   见 `known-issues.md` 的 T53。
3. 目录归一化时剥掉 B 层会发射的 flag，否则 59 个昇腾档位出现两遍 `--device npu`。
4. 分布式部署按组内最弱节点算一次统一下发，各 rank dtype 必须一致。
5. 风险：通用 flag 绑定的是引擎版本而不是厂商（vLLM 上游 `--device` 走向自动推断，具体版本需对目录钉住的镜像核对），
   B 层挂镜像系列而非厂商正是为此。

### 验收条件

- 同一模型在 Turing 节点：表单 auto → argv 与 breakdown 都是 fp16；表单 bf16 → 拒绝并给原因；extra_args 不再是第三条路径。
- 昇腾无模板部署得到与档位部署相同的 `--device npu --attention-backend ascend`。
- 多机异构组各 rank dtype 一致，有测试。
- `quirks.go` 中两条模型规则（DeepSeek、MiniMax）迁到目录档位或 A 层显式标为模型规则，不再混在硬件规则里。

## T14. docker 运行时的镜像拉取代理与超时

**状态**：2026-09-17 KV 磁盘卸载真机验收时发现（证据 `artifacts/archive/2026-09-16-kv-disk-offload/acceptance/image-acquisition-newer-vllm.md`）；未修。

### 现状

- `InstanceDesiredState.ImagePullProxy` 经 `runtime.WithPullProxy` 只落到 agent 起的 `docker pull` 命令行环境上，而 docker 运行时真正下载的是 dockerd 守护进程，CLI 的代理环境对它无效；只有 udocker/nerdctl 这类由 agent 进程自己拉取的路径才吃到这个代理。
- 镜像目录"分发到节点"的同步路径没有代理或拉取地址改写的选项，只能走节点默认出口。
- 30 GB 量级的镜像在慢链路上拉不完固定的 30 分钟超时（`prefetchTimeout`），任务按超时失败后重试从头开始。
- gpu3 上 dockerd 早已通过 systemd drop-in 配了代理（8 月 18 日），说明现场是靠手工配置绕过的。

### 建议

1. docker 运行时：任务级代理经 `docker --config` 的 `proxies` 或明确文档化为"节点级 dockerd 配置"，并在节点详情里展示 dockerd 的代理状态，避免用户以为填了 `image_pull_proxy` 就生效。
2. 节点同步路径复用 `imagecatalog/pullref` 的候选顺序（含华为 SWR 等国内源）。
3. 拉取超时改为"无进度超时"（一段时间内字节数不增长才失败），而不是总时长。

### 验收条件

- docker 节点填了任务级代理后 `docker events`/dockerd 日志能看到经代理的拉取；同步路径能按候选顺序回退；30 GB 镜像在 5 MB/s 链路上能拉完。

## T18. 完整启动信息透明化

**状态**：2026-09-19 SGLang 日志修复评审拆出，用户决定后续单独实施。

本次收尾只让日志配置 env 和启动准备脚本贯通部署预览、各 rank 与实例详情；
未增加独立配置实体、数据库列或 agent 上报协议。通用 shell 引用与 env 导出已补，
但“整份容器启动方案可复制重建”尚未实现。

后续范围与验收：

- 部署预览与实例详情完整投影同一 ResolvedLaunch：entrypoint、argv、env 及来源、
  pre-start、运行资产、挂载、容器参数。已部署实例只读持久化快照，不回最新目录重算。
- 镜像默认值差异表不能充当最终配置；空挂载编辑行不得显示成生效覆盖，半填行须明确校验。
  挂载以“宿主路径或卷 → 容器路径、读写、用途”展示，替代原始 JSON。
- agent 才能确定的设备/dGPU/网卡 env、宿主挂载路径与实际入口，从真实准备/执行路径只读上报。
  不新增可独立编辑的第二份启动配置；新增上报字段须证明不能从现有数据派生。
- 区分部署前的待确定项、已保存期望方案与实际容器/进程信息，显示来源和时刻；
  镜像内置 env、任意脚本动态 env 不得靠服务端猜测，敏感值展示和导出须遵循权限。
- 带 pre-start 的命令文本编辑已恢复：编辑器接管完整脚本，准备步骤只执行一次，返回表单
  清除手工覆盖。旧手写命令继续保留模板准备步骤。多节点方案仍需各 rank 独立导出与手动编辑语义的完整验收。
- 验证 API 字面 JSON、RTL loading/empty/error/success 与键盘操作、preview→快照→执行一致性；
  跑 make test、L2/L3、相关 race/vet，明确容器 suite 的 SKIP，不能拿期望配置冒充运行实测。

SGLang 后续兼容性：静态 dictConfig 不接收上游 configure_logger 的 rank prefix，
因此并行启动暂保留上游格式，不自动注入该配置；用户自有 logging 配置、--config、--log-file
同样保留。扩展覆盖面前需验证引擎支持的仅修改格式的机制，保留原始阈值、rank 与毫秒时间。

## T21. KV offload 产品化

**状态**：2026-09-24 立项，未实施。依据 `artifacts/research/2026-09-22-kv-offload-pd-experiment/PLAN-kv-offload.md` §12（2026-09-22 单台 H200 + GLM-5.3-Flash、coding-agent trace 的对照实验）。

### 调研结论（摘要，以原文为准）

- CPU 内存层（vLLM `OffloadingConnector` CPU tier）有效：GPU KV 池压到小于工作集时，20 分钟完成请求 187→268（+43%），重算占比 50%→27%，37% 的 prompt token 由 offload 层供给。
- 磁盘层扛不住热流量：写入 ~455 MB/s 对比 CPU 层 ~24 GB/s，热负载下命中率 0%；把 CPU 层压到 32 GiB 后磁盘层有命中，但有效读 ~76 MB/s，从盘恢复比重算还慢。磁盘层只适合冷会话存档。
- GPU KV 池充足时（`kv_cache_usage_perc` 60%、`preemptions=0`）offload 不产生任何收益。
- CPU 层由 `/dev/shm` 承载（默认只有内存的 50%）；`--ipc=host` 下被杀引擎的 `/dev/shm/psm_*` 段会残留。

### 建议

在已有 KV 磁盘卸载受管挂载（`artifacts/archive/2026-09-16-kv-disk-offload/plan.md`）之上：

1. 支持 CPU 内存层配置，默认容量按热工作集推导（约 2–4 × GPU KV 池），并校验 `/dev/shm` 容量与主机内存余量。
2. offload 是否开启由 KV 池压力（占用率、淘汰/抢占、重算占比）判断，先在实例详情给出建议，再考虑自动开启。
3. `promotion_allocation_failures > 0` 作为"CPU 层过小"的告警信号；UI 把磁盘层标注为冷存档、恢复慢。
4. agent 在引擎退出后清理残留的 `/dev/shm/psm_*` 段，引擎运行时不得清理。

### 验收条件

- 部署表单与 evaluate 能给出 CPU 层容量建议与 `/dev/shm` 不足的明确错误。
- 监控能展示 offload 命中与 promotion 失败，并在失败时告警。
- 真机复现一次调研中的对照（压力场景下开启 CPU 层后完成量提升），结果记入验收记录。

## T22. 界面上传的离线镜像包无法分发到节点

**状态**：已知问题，2026-09-25 随节点间直连同步引入；未实施。

### 现状

- 节点间镜像同步改为 agent 直连后，server 读取本地离线 blob 推给节点的路径（原 `CopyImageBlob`）随旧 relay 一并删除。
- `POST /api/v1/images/offline/import`、`/offline/import-path` 仍会登记目录并把 tar 存到 `<data>/images/`，`offline_blob_present` 仍会显示，但没有任何入口能把它发到节点；`internal/integrationtest/image` 的 `TestOfflineBlobRelayLoadsTargetWithoutSourceNode` 因此 skip。
- HTTP 归档（节点直连或 server 中转）、Registry 拉取与节点间直连同步不受影响。

### 建议

恢复为用户显式选择的独立来源（如 `source: "offline"`，由 server 流式 `docker load` 到目标），不作为节点间同步失败后的兜底；UI 只在 `offline_blob_present` 时提供该选项。

### 验收条件

- 离线包导入后可在镜像页选择 server 离线包分发到节点，行记录来源为 offline。
- 恢复并启用上述集成用例；l2 覆盖一次导入到 `present` 的完整路径。

## T28. 昇腾 PD：NPU 网络拓扑与 KV 池路线

**来源**：2026-09-29 PD 昇腾验收（`artifacts/archive/2026-09-28-pd-disaggregation/evidence/ascend-mooncake-store.md`）。

- `vllm-ascend-mooncake` 的 KV 走 NPU 自带 RoCE 网口。实验室两台 910B4 没有交换机，按卡号点对点直连（机器 A 的 N 号卡对机器 B 的 N 号卡），同一台机器内的 NPU 口之间不通；平台不知道这个拓扑，放在不互通的两张卡上时交接会在 `HcclCommPrepare` 失败。NPU 口的对端可用 `hccn_tool -i N -lldp -g` 读到。
- 主机 RoCE + Mooncake KV 池路线已手工验证（Qwen3-8B 6.4k 提示 D 首 token 1200 → 440 ms），能绕开 NPU 网络，但依赖引擎补丁（store worker 的 NPU 中转）、Mooncake master/metadata 附属进程和宿主机 rdma-core 挂载。
- 2026-09-30 同条件对比（Qwen3-8B、16K 提示、两台的 3 号卡，`evidence/ascend-transport-comparison.md`）：D 首 token 中位数为主机 RDMA 单链路 1000 ms、NPU 直传 1132 ms、主机 TCP 1263 ms，本地重算 2.2–2.4 s。即 KV 池路线性能不弱于 NPU 直传，价值在适用面（不依赖 NPU 参数面互通）。两项待解决：NPU 直传速率在 2.4–8 GB/s 间无规律波动（NPU 口计数器证明数据确实走了 NPU 口，瓶颈在传输路径；2026-09-30 同一块 NPU 口上 HCCL 集合通信实测 20.5 GB/s，见 T30 的证据，相差 8.5 倍，进一步坐实链路不是瓶颈）；主机 RDMA 多链路在点对点直连下会因交叉配对失败而周期性暂停 rail（30 s）、传输退化到 5 s，单链路则稳定在线速，多链路产品化前必须限制建连的网卡对。

### 验收条件

- agent 上报 NPU 口的 LLDP 对端，昇腾 PD 放置时校验 P、D 的 NPU 互通，不通时评估即报错。
- 对 KV 池路线做出是否产品化的决定。

## T31. 选错卡对的部署启动后才失败

**来源**：2026-09-30 加速器参数网络（`artifacts/archive/2026-09-30-accelerator-parameter-network/plan.md` §10 明确不做的部分）。

- 加速器 RDMA 网络只声明"这些节点的加速卡网口连在一起"，不记录哪张卡通哪张卡。两台 910B4 按卡号直连，只有同号卡互通；调度给两个 rank 选了不同卡号时，通信域照样建成，第一次集合通信才失败（`EI0006`）。
- 卡连通性测试（`card_matrix`）能测出这张图并给出"两端必须选相同卡号"的结论，但结果只给人看，放置不读它。
- 普通多机部署不能逐 rank 钉卡：`placement.gpus` 对每个节点都生效，恰好能表达"各节点同一卡号"，表达不了别的组合。PD 分离可以逐实例钉卡。

### 验收条件

- 多机组或跨机 PD 选中的卡对在最近一次卡测试里不可达时，评估给出错误或至少给出警告，并指出可用的卡对。
- 没有测试结果时不拦，只提示先跑卡连通性测试。

## T32. 卡连通性测试结果的展示方式待定

**来源**：2026-09-30 加速器参数网络（`artifacts/archive/2026-09-30-accelerator-parameter-network/acceptance.md`）验收后的讨论。

最初的设想是：结果先按节点对展示（全通绿色、部分通黄色、全不通红色），点进一个节点对才看卡和卡之间的明细。实现与它有两处出入：

- 只有一个节点对时（两节点的全互测或环形），界面直接显示卡矩阵，没有节点对那一层。
- 形状分了五档：全互通（绿）、仅同号卡（蓝，算通过）、部分可达（橙）、全不通（红）、未测（红）。"仅同号卡"在直连或按卡号分平面的组网下是正常形态，所以单独拎出来并算通过；但按"通了多少对"看，它只通了 8 / 64。

### 待决策

- 单个节点对时是否也先显示节点对一层，再点进去看卡。
- "仅同号卡"是保留为单独一档，还是并入"部分通"显示黄色。后者会让健康的直连网络整屏黄色，前者需要用户理解多出来的一档。

### 验收条件

- 决定后，server 读出的形状（`card_shape`）、节点对状态与界面颜色三者一致；两节点与多节点的交互路径相同或差异有明确理由。

## T34、T35、T37. ninfer 试用中暴露的获取与运行时缺陷

**来源**：2026-10-01 ninfer 试用（`artifacts/work/2026-10-01-deploy-progressive-disclosure/plan.md` 的“ninfer 真机试用对照”，逐条记录见 `artifacts/research/2026-10-01-ninfer-5090-trial/`）。

- T34：gpu3（内网）→ compshare 5090 容器实例（公有云）的镜像同步报 `send peer image archive: dial model sync target: 10.42.14.163:34787 … no route to host`。两个 agent 都经 agent_dial 连着 server。
- T35：上述失败与随后的 `.tar.gz` 导入失败期间，部署 245/246 均停在 pending，无实例、无消息。失败原因缺失的部分已于 2026-10-02 修复并迁出；剩下的是重试入口。
- T37：`internal/agent/app/modelfiles_rpc.go` 只接受 config.json + .safetensors/.gguf/.bin/.pt 或 Mistral 原生布局。

### 验收条件

- 跨网络节点之间的镜像同步能成功（经 server 中转）；部署在获取失败时显示原因（已完成）并可从该处重试。
- 非 HF 格式的模型在登记时即给出“能否部署、需要什么引擎”的结论，不在下载结束后才失败。

## T39. 运行中实例存活检查的后续

**来源**：2026-10-01 修复 T26（agent 不上报启动后退出的容器）时留下。

- 每轮按实例各调一次 inspect 的开销已于 2026-10-02 解决并迁出（一轮一次运行中容器列表）。遗留：nerdctl 的 `ps --format {{.Names}}`（不带 `-a`）只在 CLI fixture 上验证过，未在真机上核对它与 `inspect` 的 `.State.Running` 一致；不一致时只会退回逐个 inspect，不会误判实例退出。
- 引擎进程还在但健康检查失败（容器 running、`Health` 为 unhealthy，或 HTTP 健康端点不通）不处理，只处理退出。
- 容器退出后、检查之前 agent 恰好重连时，desired state 重放会按原实例在本地重启容器，不上报 failed、不计入重试预算（原有行为）。

### 验收条件

- 在昇腾 nerdctl 节点上核对：运行中、已退出的容器各一个，`nerdctl -n gpuworks ps --format {{.Names}}` 只列出前者。
- 对“进程在但不健康”做出是否处理的决定。

## T42. 云实例列表完整性有两处未经真机确认

**来源**：2026-10-01 修复 T33（云厂商已释放的实例仍留在平台实例列表里）时留下。

同步现在会删掉云端列表里没有的实例行，并把关联节点的带外恢复置为 `stopped`，因此驱动的 `ListInstances` 必须“完整或报错”。两处只能靠真机确认：

- CompShare 的列表请求不带 Region；不带时若只返回默认区域，跨区域账号的列表就不完整。翻页的 Offset / TotalCount 语义按 UCloud 惯例推断，推断错了只会在超过 100 台时报错，不会误删行。
- RunPod、Vast 的列表按“不分页”处理，未离线验证。

### 验收条件

- 用跨区域、超过 100 台实例的 CompShare 账号核对一次同步结果与控制台一致；确认 RunPod、Vast 的列表接口是否分页并记下结论。

## T43. `instances.schedule_expl` 承载启动事实

**来源**：2026-10-01 store 列清理（`artifacts/archive/2026-10-01-store-typed-columns/plan.md`），ADR 0032 后果一节。

- `planning.ScheduleRecord` 一半是给人看的放置解释（`explain`、`breakdown`），一半是后续决策依赖的启动事实：参数网络（重建的对端必须同网，PD 交接校验比对其类型）、PD 端点（网关与多机 followers 要连）、unit 归属（`planning.MatchUnits` 按它认领 unit、按 unit 计重试预算）。
- 迁移 111 为网络类型改名用 `json_set` 改写了 `$.network.parameter.type`，即数据库按内部字段做了局部更新，这正是 ADR 0032 第 2 条不允许 JSON 列承担的事。
- 迁移 118 把参数网络的键 `network.parameter` 改为 `network.compute`（[改名计划](artifacts/archive/2026-10-02-compute-network-rename/plan.md)）：在 Go 里整列读出、按原始 JSON 对象解码、只动这一个键后整列写回，数据库不看内部字段；但它仍是一次针对启动事实的形状迁移，是本项又一个例子。
- 记录写入后不再变化，所以至今与解释放在一起；但读取它的已经不只是展示。

### 待决策

- 是否把启动事实（参数网络、PD 端点、unit key / followers）提成 `instances` 的列，`schedule_expl` 只留解释快照；或者维持现状，并约定这些字段不再被迁移改写。

### 验收条件

- 决定后，控制器与网关读取启动事实不再依赖解码放置解释；若提列，迁移带旧数据回填测试。

## T44. `deployments.node_ids` 是关系却存成 JSON

**来源**：2026-10-01 store 列清理（`artifacts/archive/2026-10-01-store-typed-columns/plan.md`），ADR 0032 决策 3。

- 列里存的是 `nodes` 行的 id，驱逐检查（`store.checkModelFileEviction`）、调度（`controller/unit_nodes.go`、`planning/units.go`、`planning/replica_units.go`）与 DTO 都要回答「哪些部署用到了这个节点」或「这个部署有哪些节点」，按 ADR 0032 的判据属于关系。网关早已不读它（`proxy` 的 `TestDistributedSelectionDoesNotReadTheNodeSet` 钉住，2026-10-02 核实），原先列为读取方是过时的。
- 现在只能先把候选部署行读出（驱逐检查读该模型的全部 active 部署），逐行解码后在 Go 里过滤。手写解码已收敛到 `Deployment.StoredNodeIDs` / `NodeIDList`，各自对空值与坏数据的处理保持不变（驱逐 fail closed、DTO 视为无列表）。
- 2026-10-02 副本放置 S2-2：单机非 PD 部署钉住的节点已另存为节点条件 `gpuworks.ai/node`（表 `deployment_node_requirements`），这类部署的 `node_ids` 在 S2-3 后不再是候选集合；多机部署没有 `units` 键时 leader 身份仍只在数组下标里，改表前必须先回填 `units`。

### 待决策

- 是否改为 `deployment_nodes(deployment_id, rank, node_id)` 关联表，`node_id` 主节点列是否随之退化为 rank 0。

### 验收条件

- 决定改表时：迁移回填并对坏数据有明确去向（当前坏数据在驱逐路径上 fail closed，迁移不能让它变成「不引用任何节点」）；四个读取方的既有行为有测试钉住。

## T46. `deployments` 上违反 ADR 0032 的三列

**来源**：2026-10-01 store 列清理（`artifacts/archive/2026-10-01-store-typed-columns/plan.md`）补列注释时逐列追到写入方所见。只做了静态追踪，动手前需逐项核实。

- **`deployments.env` 有读者无写入方**：只在 `api/instance_launch_facts.go` 被读，读到的恒为空；环境变量现在写在 `container_overrides` 里。同一事实留了两个位置，其中一个是空的。
- **`deployments.placement` 存的是请求原文**：写入的是请求 map 的 JSON，而非由 `planning.Placement` 序列化的值，未知键会被持久化；卡号存在 `gpus` / `gpu_indexes` 两种拼写；`planning.WithPD` 在 map 层面读改写并保留未知键。列的 schema 实际由「历次请求里出现过的键」决定，而不是由归属类型决定。
- **`deployments.container_overrides` 同样存请求原文**：未经 `deploy.ContainerOverrides` 序列化。

- **`placement` 装了几类不同的事实**（2026-10-02 副本放置计划讨论，`artifacts/archive/2026-10-01-replica-placement/plan.md`）：用户意图（`mode`、`node_uuid`、`node_labels`）、确认后的方案（`gpus`、`tp`、`pp`）、PD 角色（`pd`）与多机副本列表（`units`）在同一列；读取先解成 `map[string]any` 再逐字段尽量读（`placementFromDocument`）；改副本接口对 `units` 读改写。部署列出多机副本后，`replicas`、`node_ids` / `node_id` 都可由 `units` 推出（与 T44 相关）。副本放置 S2-2（2026-10-02）已把 `node_labels` 迁到表 `deployment_node_requirements`（迁移 120），`mode`、`node_uuid` 两个死键删除并在写入时丢弃；其余键在这里一并处理。

### 待决策

- `env`：删列（带迁移与旧数据测试，旧行若有非空值需并入 `container_overrides` 或确认可弃），或恢复写入方。
- `placement` / `container_overrides`：写入前先解码为归属类型再序列化（未知键在入口被拒或被丢，需定策略），旧行里的未知键与旧拼写读时兼容还是迁移归一。

### 验收条件

- 三列各自只有一个写入入口，写入的是归属类型序列化的值；旧数据的每种已知形状有测试覆盖。

## T47. store 里其余疑似违反 ADR 0032 的列

**来源**：同 T46。只做了静态追踪，动手前需逐项核实。

- **没有具名归属类型**：`benchmark_runs.search`（多处匿名结构体各自解码）、`benchmark_points.long_tail`（`map[string]any`，agent 与 api 各有一份生成代码）。
- **存的是请求原文**：`benchmark_runs.dataset` 除 agentic 类型外存的是客户端发来的字节，未知键会被持久化。
- **一列装两类条目**：`image_versions.registry_sources` 同时装目录声明的仓库引用与运维添加的归档 URL，由 `preserveClusterArchiveSources` 按类别合并。

### 验收条件

- 每一项要么按 ADR 0032 处理（给出具名归属类型、写入方收敛到它、两类条目分开存），要么在列注释里写明维持现状的理由。

## T48. `instance_events.data` 不满足 ADR 0032 第 2 条

**来源**：2026-10-01 store 列清理（`artifacts/archive/2026-10-01-store-typed-columns/plan.md`）的合并请求评审。

- 新行的 `data` 是事件 code 的插值参数，`store.addEventTo` 把 `map[string]any` 直接序列化进去：每个 code 带哪些键只由各调用点决定，没有具名归属类型。
- 旧行（事件带 code 之前写下的）是自由文本，与新行同列，一列两形态；这部分随 retention 淘汰，与 T45 同时消失。
- `Store.AddInstanceEvent(…, data string)` 这个写自由文本的入口已无生产调用方。

### 待决策

- 给每个事件 code 的参数一个具名类型（客户端按 code 插值，参数集合本来就是按 code 固定的），或在列注释里写明维持 `map[string]any` 的理由。
- 无调用方的 `AddInstanceEvent` 是否删除。

### 验收条件

- 每个事件 code 写入的键集合有一处定义并有测试；或维持现状的理由写进列注释与 ADR 0032。

## T49. 混用 GPU 架构的 PD：attention backend 不一致

**来源**：2026-09-30 实例级生命周期 A5 真机验证（`artifacts/archive/2026-09-30-instance-lifecycle/evidence/a5-pd-multinode-sleep.md`）。

gpu3（Quadro RTX 8000、RTX 2080 Ti，Turing）与 4090 节点（Ada）组成的 `vllm-nixl` 部署（vLLM 0.25.1）：4090 上的 rank 自动选 `FLASH_ATTN`，Turing 卡只能用 `TRITON_ATTN`（FlashAttention 不支持 sm_75）。NIXL 握手时两边的兼容哈希不同（`NIXL compatibility hash mismatch`），每个请求都 `KV load failure`；`kv_load_failure=fail` 下返回 500，fallback 也救不回来。所有实例加 `--attention-backend TRITON_ATTN` 后交接正常。多机副本内部同样受影响（一个副本的 rank 分在两种架构上，KV 布局也要一致）。

评估时平台只给 `PD_GPU_MODELS_DIFFER` 警告，看不出会必然失败。引擎自动选 backend 的规则随 vLLM 版本和卡的算力而变，平台不能只按"卡型号不同"判断。

### 验收条件

- 平台为 PD 部署的所有实例定出同一个 attention backend（各实例的卡都支持的那个），写进启动参数；用户显式指定的不覆盖，但不一致时评估报错。
- 或者至少：评估能判断出各实例会选不同 backend 时报错并提示加 `--attention-backend`，而不是只给型号不同的警告。
- 覆盖 NVIDIA 同代混卡（不受影响）与跨代混卡（受影响）两种输入。

## T55. 模型页与存储页仍取全量节点列表

**来源**：2026-10-03 修 T52 时留下；其余页面（部署表单、部署详情、镜像、日志、推理模板详情、网络两处）已于同日改为按页搜索与服务端分面，见 `artifacts/archive/todo-resolved/2026-10.md`。

- 模型页（`ModelsPage`，每 15 秒）：登记/下载/登记路径三个弹窗的节点选择、节点间同步弹窗的目标与来源，以及 `buildModelPrompt` 把全体节点（含 GPU 明细）写进 AI 助手的提示词，且每一行都在渲染时生成一次。
- 存储页（`StoragePage`，每 15 秒）：逐节点卡片（名称、状态、`models_root` 编辑、磁盘总量）与路径用量表；`GET /api/v1/storages/usage` 本身也逐节点查询、不分页。
- 两页正被共享存储改造（`feat/nfs-1`，未合入）大幅重写，现在改会与之冲突，所以等它合入后再做。

### 验收条件

- 模型页的节点选择用 `NodePicker`（按页搜索），同步来源取自模型自身的节点行；提示词改用服务端的汇总（如 `GET /api/v1/nodes/facets`），不再列出每台节点。
- 存储页的节点与用量合并为一个分页接口；1000 台节点下两页的轮询不再随节点数线性增长。

## T59. 推理模板表达不了 KV 磁盘层与托管可写目录

**来源**：2026-10-08 编写单机 4×RTX PRO 6000 / 6000D 的 GLM-5.3-Flash TensorFold 模板时核对（master `de0cd514`，结论用 `deploy.ReverseArgs` 实测）。
与 T21 的分工：T21 关心什么时候开 offload、开多大；本条关心模板能不能把 offload 与它需要的磁盘目录**声明**出来。

**计划**：`artifacts/work/2026-10-08-template-cache-dir/plan.md`。2026-10-08 讨论后收窄了下面的建议：不扩展平台概念、先不挂载，resolver 不加主机事实。

| 能力 | vLLM | SGLang | callable |
|---|---|---|---|
| 内存（CPU）层 | 模板写 `--kv-offloading-size N`，被参数字典解成 `kv_offloading_size` | HiCache 不在参数字典里，`--enable-hierarchical-cache` 等原样透传 | 只能经 env/args 透传 |
| 磁盘层开关 | `kv_disk_offload`、`shared_kv_dir` 与两个线程数共用一个 `--kv-transfer-config` JSON，模板写它只会成为不透明残余，不解成字典字段 | 只能透传 `--hicache-storage-backend` 一类参数 | 无 |
| 磁盘层挂载 | 只有部署表单设了 `shared_kv_dir` 才生成托管挂载（`deploy/kv_disk.go`：宿主目录由 agent 定、配额默认权重的 2 倍、agent 清理器控量） | 无 | 无（ADR 0036 后果一节已写明暂不提供） |

由此：

- 模板若硬写磁盘层参数，引擎会写进容器可写层：容器重建即丢，没有配额与清理，平台不知道它占了盘。
- spec 没有任何挂载或可写目录字段；需要落盘缓存的 callable 引擎（例如 TensorFold 的 `--snapshot-dir` 前缀快照）只能写进镜像文件系统。
- resolver 的入参只有 GPU 事实，模板不能按主机内存定 CPU 层大小，也不能按数据盘空间定磁盘层大小；`kv_offloading_size` 只出现在 PD 告警里，不进调度的主机内存账。
- 托管挂载根默认是 agent 状态目录下的 `runtime-mounts`（`GPUWORKS_RUNTIME_MOUNT_ROOT` 可改）。AutoDL 这类 `/opt` 在 30 GB 系统盘上的节点，即使从表单开了磁盘层也会写系统盘。

### 建议

按 ADR 0036「路径 + 容量上限成对加入词表」的预留扩展：

1. spec 加一个所有引擎通用的托管目录声明，例如 `runtime_dirs: [{purpose: kv_disk, mount_path: /kvcache, limit: {weight_multiple: 2}}]`，复用 `MountPurposeKVDisk` 的宿主目录选择、配额与清理器；部署表单的 `kv_disk_limit_gib` 仍可覆盖容量。
2. 平台给出 `${KV_DISK_DIR}`、`${KV_DISK_LIMIT_BYTES}` 这类变量，模板自己摆进 args/env；vLLM 的 `--kv-transfer-config` 由此也能在模板里写成带变量的形式，并能被识别为磁盘层。
3. resolver 入参补主机内存与可用数据盘空间；调度把 CPU 层大小计入主机内存。
4. 节点侧给托管挂载根一个落在数据盘的默认值，或安装时探测数据盘。

### 验收条件

- 一个 callable 模板与一个 vLLM 模板能各自只靠 spec 声明出磁盘层：部署后宿主上有平台托管的目录、配额生效、删除部署后目录被清理，容器重建后数据仍在。
- 导入时拒绝引用了未声明目录的变量；同一部署的表单设置与模板声明冲突时有明确的错误或覆盖规则。
- 两个各开 CPU 层的实例不会被放到主机内存放不下的同一节点。

## T69. 镜像 e2e 夹具起的私有 dockerd 会删掉宿主机的 docker0

**来源**：2026-10-17 分析 ppu1 docker0 被删（外部的 `gpuworks-ppu-r05-docker.service` 以 `--bridge=none` 启动所致，非本仓库代码）时，核对仓库里同样用法的地方。

- `test/e2e/image_runtime_fixture.py` 用 `sudo -n unshare --mount ... dockerd --bridge=none --iptables=false ...` 起私有 dockerd，没有 `--net`。moby 在禁用默认网桥时调用 `removeDefaultBridgeInterface` 删除当前网络命名空间里的 docker0，因此会删掉宿主机系统 docker 的网桥；系统 dockerd 继续运行、看起来正常，但 bridge 网络的容器全部断网，新容器报 `adding interface vethX to bridge docker0 failed`。
- 只在手动跑 `make test-e2e-image-runtime`、`test-e2e-image-registry-*`、`test-e2e-image-in-use`、`test-e2e-image-transfer-recovery` 等目标时触发，不在 CI 中。
- 直接加 `unshare --net` 会让 registry-pull / registry-mirror 失败：用例在测试进程里起的仓库监听宿主机 `127.0.0.1:<端口>`（`test/e2e/image_registry.py`），独立命名空间里的 dockerd 访问不到。归档拉取（agent 下载后 `docker load`）不受影响。

### 建议

dockerd 进独立网络命名空间并启用回环；在命名空间内用与宿主机仓库相同的端口在 `127.0.0.1` 上起转发，经共享文件系统上的 unix socket 接回宿主机仓库，镜像引用与用例不变。不采用「宿主机有 docker0 就拒绝启动」：几乎所有开发机都有系统 docker。

### 验收条件

- 在装了系统 docker 的机器上跑 image-runtime、registry-pull、registry-mirror 用例全部通过，跑前跑后 `ip link show docker0` 一致，系统 docker 的 bridge 容器网络不受影响。

## T71. bench 的 tokenizer 绕过拉取代理与来源规则

**来源**：2026-10-17 模型拉取来源与代理工作（`artifacts/work/2026-10-17-model-pull-sources/plan.md`）收尾时核对仍直连公网的取数点。

- `pkg/bench/tokenizer/ref.go` 由 agent 用 `http.DefaultClient` 直接请求 huggingface.co，不读节点的拉取代理（[ADR 0050](docs/adr/0050-model-pull-sources-and-proxies.md) 决策 5）、站点规则的替代地址与禁止回源，也不带站点凭据。
- 影响：节点必须经代理或镜像站才能出网时，评测取不到 tokenizer；全离线集群里「禁止」规则对它不起作用。

### 建议

- 让取 tokenizer 复用 `internal/agent/modelfetch` 的候选与冷却，候选由 server 随评测任务下发；gated 仓库带站点凭据。
- 或评测时优先读模型目录里已有的 tokenizer 文件，只在缺失时才联网。

### 验收条件

- 节点只能经拉取代理出网、或只有内网镜像站时，评测能取到 tokenizer；站点被「禁止」规则关闭时评测直接报错并说明原因。

## T72. ModelScope 模型 logo 不在来源规则之内

**来源**：2026-10-17 同上。

- ModelScope 返回的 logo 是任意 CDN 地址。管理节点取 logo 经「用于管理节点出站访问」的代理，但不套站点规则，也没有候选回退。
- 管理节点连不上该 CDN 时静默没有 logo，用户看不到原因，也没有地方可以配置。

### 建议

- 先确认是否值得做：logo 只影响展示。若做，让 logo 域名可被站点规则匹配，或在模型卡上标明「logo 不可用」。

### 验收条件

- 管理节点无法访问 logo CDN 时，界面有明确提示或可通过规则配置到可达地址。

## T78. runveil 下 force 删除部署不回收容器 rootfs

**来源**：2026-10-20 DeepSeek-V4.1-Flash-1017 真机验收（`artifacts/work/2026-10-19-frontier-model-templates/validation.md`）。

- 每个 runveil 实例有一份约 33 GB 的容器根文件系统；force 删除部署后目录仍在，几轮实验后 700 GB 盘只剩 21 GB。
- 新实例创建失败时只报 `clone cached base rootfs: exit status 1`。

### 建议

- 实例从期望状态中消失时由 agent 删除对应容器目录；失败信息带上底层原因。

### 验收条件

- 连续 force 删除并重建部署，节点磁盘占用不随次数增长。

## T79. 驱动过旧时只报 `No accelerator`

**来源**：同上。

- 主机驱动 550.54.14，`lmsysorg/sglang:dev-dsv41` 为 CUDA 13（`NVIDIA_REQUIRE_CUDA cuda>=13.0`），引擎启动后才失败。
- 镜像自带的 `/usr/local/cuda-13.0/compat` 前置到 `LD_LIBRARY_PATH` 后可在该驱动上运行。

### 建议

- 部署评估时比对节点驱动与镜像的 CUDA 要求并提示；forward-compat 作为显式选项，而不是静默启用。

### 验收条件

- 驱动不满足镜像要求时，部署前或启动失败时给出"驱动过旧"的明确原因。

## T82. PRoot 公平调度补丁让 SGLang 权重加载卡住，旧线程饿死问题只靠线程上限缓解

**来源**：2026-10-20 runveil 多线程工作负载（`artifacts/work/2026-10-20-runveil-many-threads/plan.md`，含补丁、本地压力测试、真机 A/B 表与证据）。

- 上游 PRoot 每轮 `waitpid(-1)` 只取一个事件，Linux 又从列表头（最新线程）开始返回，上千线程时旧线程长时间停在 tracing-stop。补丁 `proot-fair-wait.patch`（取尽待处理事件、先被跟踪的先处理、pid 哈希表）在本地压力测试里修好了，真机 8×H200 上初始化也快了数倍，但 SGLang 权重加载阶段每次都卡住（有无线程上限都卡），所以没有内嵌。
- 当前缓解是服务端给 runveil 节点上的 SGLang 部署默认下发线程池上限（`OMP_NUM_THREADS` 等，在预览里可见、可删除），同样的取值在 dep29 上验证可加载。线程仍可达数百，PRoot 仍是单线程瓶颈。
- 卡住的原因未查明，plan 里列了三个推测和区分办法。如果重做，arm64 的内嵌 PRoot 也要在 arm64 机器上重建（开发机没有 arm64 Linux 或 qemu binfmt）。

### 建议

- 先只保留哈希表和延迟释放、去掉批处理，真机跑权重加载；再试每轮限 16–32 个事件、按到达顺序处理；卡住时抓 `/proc/<tid>/syscall`、`stack` 与 proot 的 `perf`。压力测试里补一个“一个线程持锁做路径系统调用、其余在 futex 上等”的依赖链用例。

### 验收条件

- 补丁（或其简化版）在 8 卡 SGLang 权重加载上不比原版慢，且 `stress/run-stress.sh` 里主线程不再饿死。

## T83. 内嵌 PRoot 没有 temp-noise 补丁的效果，无害报错盖住真正的失败原因

**来源**：2026-10-20 复核 Vast 卡塔尔部署 26（`artifacts/work/2026-10-19-frontier-model-templates/validation.md`）。

- PRoot 删除临时目录（绑定到镜像里不存在的路径、过长的 unix socket 路径都会建）后要切回原工作目录；降权后切不回 `/root` 就报
  `can't chdir to '/root'`，不影响退出码。`build/runtime-assets/proot/patches/proot-temp-noise.patch` 正是为它写的，但内嵌的
  x86_64 二进制仍会打出这句，`build.sh` 的补丁路径此前也指向不存在的 `../patched/`。
- !548 让 PRoot 从 `/` 启动后这句不再出现，但 PRoot 其他报错仍可能在容器失败时排在最后。

### 建议

- 核对内嵌 PRoot 的构建来源，按 `build.sh` 重建（带 temp-noise 补丁）并过门禁。
- agent 取容器失败原因时跳过 PRoot 自己的 `proot error:`/`proot warning:` 行。

### 验收条件

- 内嵌 PRoot 在上述复现步骤下不再报错；容器因引擎错误退出时，失败原因是引擎的报错。

## T100. 经来源规则拉取的镜像，从节点删除后仍留着替代地址的 tag 与全部内容

**来源**：2026-10-20 nerdctl 拉取修复的真机验证（`artifacts/archive/2026-10-20-t73-nerdctl-pull-progress/evidence/real-machine-2026-10-20.md`）。编号跳到 T100，避开并行分支可能占用的 T83 起的编号。

- 来源规则让节点从替代地址拉取（例如 `registry.zstack.ai/gpuworks/gpuworks-runner:hggc13.0-vllm0.23.0-r2`），拉完后 agent 给它打上规范名的 tag。
- 在镜像页对该节点「从节点删除」只删了规范名的 tag，替代地址的 tag 还在，镜像内容与快照（这次 11 GiB）都没释放，只能手工 `nerdctl rmi`。
- 在 nerdctl 节点上观察到。2026-10-20 评审复验（`artifacts/archive/2026-10-20-t73-nerdctl-pull-progress/evidence/real-machine-review-2026-10-20.md`）在 docker 与 udocker（native）节点上同样残留：docker 留下 `127.0.0.1:5098/library/golang:1.26`，udocker 留下 `repos/127.0.0.1:5098/library/golang`。

### 建议

- 删除时同时删掉同一镜像（按 digest）在节点上的其他 tag，至少是平台自己经来源规则拉取时留下的替代地址 tag；不删用户自己打的、与平台无关的 tag。

### 验收条件

- 经来源规则拉取的镜像从节点删除后，节点上不再有该镜像的任何平台 tag，内容与快照被回收（`df` 与 `nerdctl images` 可见）。
- 测试覆盖 nerdctl、docker 与 udocker 三种驱动的删除路径，夹具忠实还原替代地址 tag 的存在。

## T105. runveil 非交互 Exec 未恢复工作负载环境与挂载

2026-10-24 在 910B4 dGPU 真机验证中，vLLM 已到 health，但 `Driver.Exec` 执行的 Python 导入 `torch_npu` 报 `libascend_hal.so` 不存在。`Exec` 只向 `runveilCommand` 传入 Name，未恢复运行中的 Env、Mounts、GPUIndexes；改用已恢复持久化 spec 的 `ExecPTY` 后，同一诊断成功看到 8 GiB 配额、拒绝 9 GiB 申请，随后真实 chat 通过。

修复应让非交互 Exec 复用工作负载 spec，并覆盖持久化恢复、缺失 spec、挂载路径、设备选择及命令错误传播。此次未修改该独立执行接口。

## T106. CANN 8.5 runveil 的 DCMI 初始化阻塞

2026-10-24 在 910B4 / driver 26.0.rc1 上补测 `cann8.5-910b-vllm0.18.0-r2`：修复 ARM64 PRoot 路径解析后，引擎仍停在 `set_device`，没有达到 healthy。镜像无 `/.dockerenv` 和 `/usr/bin/systemd-detect-virt`；DCMI 容器检测 fork/exec 失败后调用 `exit`，子进程卡在 CANN dump 析构的锁，父进程等待其退出。CANN 9.0 / vLLM 0.20.2 的同机默认路径已通过。

临时绑定 `/.dockerenv` 的单变量对照绕过了死锁，但随后 `TsdOpen(devId=7)` 返回 31、`set_device` 报 507033。已通过实际二进制反汇编与查询对照定位：TSD 在将用户设备号 0 映射回逻辑卡 7 之前查询芯片型号，因逻辑卡 0 不存在而返回 87。仅将 TSD 这次失败的型号查询重试到真实卡 7 后，算子、8 GiB 配额、9 GiB 分配拒绝和随后真实 chat（HTTP 200）均通过。该 hook 和容器标记仅用于临时诊断，没有进入 agent 或镜像。下一步需修正检测阻塞及 TSD 转换顺序，并在默认生产路径重新验收，不能将局部兼容对照标为默认支持。
