# ADR 0010: 推理目录采用模型同目录的可执行多文件包

日期: 2026-08-31 · 状态: 已接受(镜像不可变引用条款被 ADR 0017 取代,改为纯 tag;多文件包结构、`assets/` 限制、权重/依赖不可变引用要求本身仍然有效)

ADR 0009 的扁平档位继续作为产品裁决单元,但单个 `catalog.yaml` 不再足以表达真机验证中必需的补丁、运行包装器、多模型组件、动态拓扑与 API 能力。内置目录改为一个由根 manifest 和 `models/<model_id>/model.yaml` 组成的多文件权威包;模型只能引用自身 `assets/` 中的轻量文件,镜像、底层依赖和权重仍通过不可变外部引用交付。目录包整体 `go:embed` 进 server,在线同步、离线导入和内置 seed 使用同一包格式,数据库仅作投影。这样保留单一事实源与单二进制分发,同时避免 base64 膨胀、跨目录引用和把测试机环境伪装成可复现配置。

运行时拓扑由无 I/O 的受限求解器根据实际 GPU 集合产生标量变量;多权重关系由显式运行组件及 primary/speculative_draft/verifier 角色表达,但只有上游命令显式引用外部 checkpoint 时才拆组件。权重内置 MTP/DSpark head 的融合式 checkpoint 保持单组件,不能仅凭模型名猜出第二个 mount。部署后的调用能力由推理 API 契约表达。README 只承载人读提示,不能替代机器契约。资产包禁止携带框架、共享库、venv、权重和镜像层;缺少这些依赖时必须补齐带 digest 的镜像,不得通过扩大目录包绕过。

启动信息固定为一条单向数据流:InferenceSpec 是目录事实,ImageDefaults 是镜像运行基线,ContainerOverrides 是本次部署的字段级修改,ResolvedLaunch 是它们与部署/GPU 选择合并后的唯一最终启动方案,InstanceDesiredState 则在 ResolvedLaunch 之外增加实例身份与调度生命周期事实。旧的 RunSpec、InstanceSpec、引擎 Result、RuntimeSpec 和 Assembly 全部删除且不保留兼容别名;预览和 controller 只能共享同一个 ResolveLaunch 入口。数据库 `DesiredStateJSON` 字段只保存 InstanceDesiredState 的序列化快照。
