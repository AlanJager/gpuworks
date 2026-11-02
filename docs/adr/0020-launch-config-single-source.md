# ADR 0020: 启动配置单一事实源——一套字典化参数，命令模式是唯一特殊通道

日期: 2026-09-15 · 状态: 已接受（取代 ADR 0007 决策 3 的合并优先级；ADR 0009 决策 7 的 args 存储形态不变，导入与部署时按本 ADR 拆分；ADR 0013 的 `manual` 模式成为唯一绕过参数字典的通道。评审稿与目录统计见 `artifacts/archive/2026-09-16-launch-config-single-source/plan.md`）

## 背景

显存利用率一个参数在仓库里有三种"默认"：参数字典的表单默认 0.92、估算/调度在四个函数里各自写死的回退 0.9、引擎自身缺省时的值。表单滑条只渲染默认值而不赋值，用户不碰滑条提交的请求里就没有这个 key，于是同一次部署：屏幕显示 0.92，估算与准入按 0.9，vLLM 恰好也按 0.9，SGLang 按自己的公式。

根因不是常量没统一，而是同一份启动配置在系统内部有两种表示：类型化的 `quick_params` 与自由文本的 argv（目录 spec args、用户 extra_args）。二者只在命令拼装的最后一刻按三层优先级合并（`deploy/args.go`），估算器却在合并前只读 `quick_params` 一份。内置目录 962 个档位中 839 个在 args 里写了字典内 flag（`--dtype` 144、`--quantization` 42、`--kv-cache-dtype` 8……），这些值引擎全部拿到、估算器一个也看不见。

ADR 0007 决策 3 已经规定"导入时能映射参数字典的拆成 key-value，映射不了的逐字透传"，ADR 0009 扁平化目录时把 args 改回原始 JSON 列，导入期拆分随之消失；反解代码 `ReverseArgs` 保留至今，生产代码零调用。

## 决策

1. **参数字典是唯一产地。** `deploy/param_specs.go` 一行一个参数，持有 key、类型、控件、默认值（可按引擎分列）、别名（如 `half → float16`）、每引擎的取值 flag 与关闭 flag（布尔参数的 on/off 两种形态）。所有默认值、枚举、上下限、key↔flag 映射只从这里读；`sglang.go` 手写的映射表、`ValidateQuickParamsForEngine` 手写的枚举、前端硬编码的 key 列表、`structuredQuickFlagSet` 与 `ReverseArgs.booleanFlagValue` 里的极性例外全部删除。
2. **写入边界物化一次。** `evaluate` 与 `create` 经同一个 `deploy.Materialize`：请求字段、extra_args、目录 spec args 按"显式字段 > extra_args > 模板 args > 字典默认"合并成一份完整的类型化配置，字典内 flag 从 args 里拆进字段，字典外 flag 留作残余 args。extra_args 里的字典内 flag 搬入字段并在响应里回显 `moved`；与显式字段值冲突时拒绝（ProductError `LAUNCH_CONFIG_CONFLICT`，同时给出两个值）。此后系统内部不再有第二种表示。
3. **未设置即引擎默认，服务端不发明值。** 持久化配置只记录用户字段、extra_args 与模板陈述过的值；没有任何一方陈述的 key 保持未设置，含义是"用户没有选择，引擎自身默认生效"，argv 不发射它。参数字典的默认值是表单的初始值：前端提交它显示的每一个值，所以新建部署的 argv 与界面所见一致（vLLM 显存利用率默认 0.9，与引擎自身默认相同；SGLang 0.88）。估算所需而未设置的利用率在 planning 一处按字典默认解析（`deploy.DefaultGPUMemUtil`），估算、reserve、scheduler 不再各自回退，无效值是错误。字典标为 `Automatic` 的 key（如 `max_model_len`）由估算器从模型事实（config.json）推导，不发明数字。`dtype` 类硬件相关 key 允许 `auto` 作为显式值，如何按节点解析见 `todo.md` T6，不在本 ADR。
4. **快照是启动的唯一输入。** `Deployment.QuickParams`（完整配置）、`Deployment.ExtraArgs`（用户残余）、`CatalogSelectionSnapshot.extra_args`（模板残余）在创建时写定，实例启动只读它们，不回目录。这与 ADR 0002 一致，也是现状，只是快照内容从原始 args 变为拆分结果。
5. **读者只读这一份。** argv 组装、估算与调度、表单渲染、模板偏差判定、已部署实例的显存账本都从持久化配置取值。argv 组装的层间规则只剩两条，且都只作用于字典外的残余：用户残余覆盖平台注入的同名 flag（`--host` 等）；用户残余覆盖模板残余的同名 flag。
6. **命令模式 `manual` 是唯一例外。** 用户接管整条命令时，命令文本是真源：估算不适用并在界面明示，表单参数字段置灰；放置输入（GPU 数、TP、显存限额）仍来自表单，准入按权重加 reserve；平台仍改写 `--port` 与 `--served-model-name`。`engine` 与 `image-entry` 模式不在例外之列。
7. **目录文件格式不变。** 作者仍在 `args` 里写 argv；`ReverseArgs` 从 `inferencecatalog` 下沉到 `deploy`，导入时校验字典内 flag 的值能被字典解码，解码不了即拒绝导入。`--quantization`（引擎量化方法，如 `modelslim`）与 `weights.quantization`（权重格式，如 `int8`）是两个词汇，保留各自含义，不做一致性校验。
8. **存量数据在 server 启动时归一化，且升级不改变任何存量部署的启动参数。** `store` 不能反向依赖 `deploy`，归一化放在 `app` 启动阶段：逐部署把旧的 `QuickParams + ExtraArgs + 快照 args` 里字典内的 flag 拆进字段，只拆分、不补值，幂等，失败则启动失败；归一化前后实例的 argv 逐字相同。旧数据里字段与 extra_args 的冲突按旧语义（extra_args 胜）处理，不报错。

## 后果

- 正面：估算器与引擎读同一份值，不再靠手工同步；升级不会改变存量部署的启动参数；新增一个普通参数只改字典一行；删除的重复包括五处 0.9、SGLang 手写映射（19 项，字典同列另有 2 项手写表遗漏，正是漂移的实例）、62 行三层合并、131 行模板偏差比对、前端 46 个 key 与 16 条规则副本、`templatePrefillValues` 的 args 解析、`diskKVTransferConfig` 的 16 线程默认。
- 负面与约束：extra_args 对 API 直接调用方的语义变化（非冲突自动搬入并回显，冲突返回 400）；经界面新建的 SGLang 部署从"引擎自算 mem-fraction"变为显式 0.88，需真机验证，存量部署不受影响；界面上的布尔开关总是显式提交，新建部署的 argv 因此含 `--enable-prefix-caching`、`--enable-prompt-tokens-details` 等 flag，而 API 调用方省略的 key 保持引擎默认；目录作者需要知道 args 里的字典内 flag 会被拆进字段。
- 被否方案：运行时 resolver（每次构造估算输入时读四个来源合并，把双表示固化成制度）；argv 作为真源、估算器反解（调度器先定 TP、argv 依赖 TP、估算又要读 argv，成环）。

## 修订（2026-11-02）

决策 7 的「解码不了即拒绝导入」改为不阻止导入：只有一个 key 拥有的 flag 若带着字典解不出来的值，flag 和值原样留在残余 args 里交给引擎，导入给出 `CATALOG_ARG_PASSED_THROUGH` 警告。原因是字典总落后于引擎（新的 tool call / reasoning parser、KV 精度不断加入），拒绝导入等于让平台的词表决定模板能不能用，作者被迫绕道（例如为了用 Ascend 的 int8 KV cache 把整个档位改成 callable 引擎）。代价是这样的值不进入类型化配置，部署表单和显存估算看不到它；内置目录出现该警告时更新字典。
