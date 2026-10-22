# ADR 0015: 试验场请求按「协议定义 × 目录声明 × 引擎发现 × 用户覆盖」四层组合，并提供可直接编辑原始请求的专家模式

日期: 2026-09-05 · 状态: 已接受

## 背景

在 gpu3 上部署 `Qwen/Qwen3-TTS-12Hz-0.6B-Base` 后暴露两类问题：

1. 部署列表的「打开试验场」对所有部署硬编码跳 `/playground/chat?model=<served>`（`ui/src/pages/DeploymentsPage.tsx`），不看该部署声明的 API contract；chat 页找不到该模型就静默回退到第一个 chat 模型，URL 被改写成别的模型。推理模板详情页则按 operation 的 `renderer` 正确选路由。同一件事两套逻辑。
2. 语音合成页把请求体写死为 `{input, voice:"alloy", response_format:"wav"}`；同样的硬编码还在示例代码生成器（`inferenceapi/examples.go`）与 L3 模板校验（`templateprobe/evaluation_audio.go`）各有一份。vllm-omni 对 Qwen3-TTS 的语义是：Base 权重没有预置 speaker，只支持 voice cloning，必须 `task_type=Base` 加 `ref_audio`/`ref_text`；CustomVoice 的 speaker 是 vivian/ryan 等而非 alloy；VoiceDesign 要显式 `task_type=VoiceDesign` 加 `instructions`。于是报 `This model does not support CustomVoice task (no speakers configured)`。

根因不是某个模型，而是产品把各 `/v1/*` 端点当成参数固定的 OpenAI 协议。实际上除 chat/embeddings 外，**同一端点在不同引擎与模型上参数语义各异**：语音有 speaker/克隆/风格三种任务模式；图片有各模型不同的 size/steps/guidance；视频有 seconds/fps/分辨率枚举；realtime 有 session 配置；chat 也有 reasoning、tools、多模态 content 等模型相关开关。目录里 voxcpm2、moss-tts、fish s2-pro、higgs-audio 等都会撞到同类问题。另外，专家用户需要不受表单限制、自行构造与修改请求。

## 决策

请求组装分四层，每层只拥有一种知识，任何一层都不允许出现模型名或引擎名分支：

1. **协议定义（代码，`inferenceapi.Definition`）**：只描述 operation type「是什么」：允许的端点、方法、编码、响应类型、异步生命周期（已有），新增 **主输入槽位**（chat→`messages`；embeddings→`input`；rerank→`query`+`documents`；transcribe→`file`；speech→`input`；image/video→`prompt`；realtime→事件流）与 **发现路由**：只读 GET 路由，试验场可经 proxy 向当前部署查询，例如 speech 的 `/v1/audio/voices`、所有类型的 `/v1/models` 与 `/openapi.json`。proxy 的 `AllowsRequest` 白名单同步纳入发现路由（仅 GET）。
2. **目录声明（YAML spec `api.operations[]`）**：每个 operation 可选两个字段。`request_defaults`：固定合并进请求体（JSON）或表单（multipart）的常量，如 Base 写 `task_type: Base`、VoiceDesign 写 `task_type: VoiceDesign`。`inputs`：该 operation 需要用户额外提供的字段列表，每项含 `name`、`kind`（`text|number|select|json|audio|image|file`）、`required`、本地化 `label`、`options`、`default`；媒体类字段前端按 `encoding` 编成 base64 data URL 或 multipart part。由 `NormalizeContract` 校验：kind 在固定词表内，`name` 不得与主输入槽位或 `model` 冲突。随 contract 进入部署快照，已部署实例行为不可变。
3. **引擎发现（运行时）**：试验场按 Definition 的发现路由向部署本身取事实：voices 列表填充下拉框，`/openapi.json` 为所选端点提供字段、类型、枚举与默认值。发现失败只降级（下拉变自由文本、无 schema 提示），不阻断。**不再由代码猜任何默认值**：没有 voices 就不发 `voice` 字段。
4. **用户覆盖（专家模式）**：每个试验场页面都有「请求」抽屉，展示最终组装的请求：方法、路径、头、请求体（JSON 可直接编辑；multipart 显示字段表与文件槽位）。提供「从 OpenAPI 生成骨架」「保存为预设」「复制 curl / Python」。页面分「表单」与「原始」两种互斥模式：原始模式下文本为唯一真值，表单冻结；切回表单按表单重新生成并提示会丢弃手工修改。原始请求经同一 proxy 原样转发，server 不做任何修补，端点仍限制在该部署 contract 允许的路由内。chat 现有的「额外参数 JSON」并入此机制。

合并顺序固定为：主输入槽位（渲染器 UI）→ `request_defaults` → `inputs` 用户值 → 专家原始编辑。

三处消费方共用同一份组装逻辑：试验场、示例代码生成器、template-probe L3（`L3Case.Voice` 等模型特定字段改为通用 `request_overrides`）。Deployment DTO 增加已解析的 contract operations（type、endpoint、renderer），部署列表与模板详情页共用一个 `playgroundRoute(operation)` 决定入口路由。

## 备选方案与否决理由

- **server 内按引擎/模型写适配器**：每加一个模型就要改 Go 代码，与「参数字典由 server 下发、不硬编码」的规约相悖；否决。
- **纯 OpenAPI 驱动表单**：vLLM 的 `/v1/audio/speech` schema 是所有 TTS 模型字段的并集，无法表达「Base 必须带 ref_audio」这类模型级约束，也不带本地化文案；OpenAPI 只作为专家模式的提示与骨架来源，人工策划的 `inputs` 仍是表单真值。
- **只加一个「额外 JSON」文本框**：解决不了文件类输入（ref_audio 需 base64 或 multipart），也修不了入口路由；作为专家模式的子集保留。

## 影响

- 兼容：旧 YAML 不写新字段照常工作；示例与 L3 在无声明时只发主输入槽位。
- 数据：5 个 Qwen3-TTS spec 补声明；复核目录内其他 TTS/图片/视频 spec 的参数语义。
- 安全：专家模式不放宽 proxy 白名单，仅新增只读发现路由；外部（设置里的 LLM）模型仍只允许 chat。
- 测试：`inferenceapi` 校验与示例快照；proxy 白名单含发现路由与拒绝非 GET；DTO 字面 JSON round-trip；RTL 覆盖入口路由、声明式 inputs 渲染、发现降级、原始模式原样发送；template-probe L3 组装。
- 后续：用户级预设先落 localStorage，「保存为推理模板」把当前专家请求写回用户来源 spec 的 `request_defaults`，形成专家用法回流目录的闭环。
