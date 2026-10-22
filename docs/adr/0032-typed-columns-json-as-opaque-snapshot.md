# ADR 0032: 持久化事实默认用带类型的列，JSON 列只做整体读写的不透明快照

日期：2026-10-01 · 状态：已接受

依据：[持久化事实默认用带类型的列](../../artifacts/archive/2026-10-01-store-typed-columns/plan.md)。

## 背景

`store` 的实体里有七十多个 `type:text` 列，大多数存 JSON。往 text 列里写一段 JSON 不需要迁移、不改表结构，
是新增一个事实阻力最小的写法；代价是这部分 schema 只存在于 Go 代码里：数据库约束不了它，迁移看不见它，
同一列可以被不同写入方写成不同形状，读取方各自解码、各自处理坏数据。查询一旦需要看进 JSON，就只能在 SQL
里做文本匹配或 `json_extract`，结果依赖序列化细节。

## 决策

1. **新增持久化事实必须谨慎，存储形式不是豁免。** 新增 JSON/text 列、往已有 JSON 里加 key，与加列加表同等对待：
   只有确实必要、且无法从现有数据推断或计算得到时才加。
2. **默认用带类型的列或表。** JSON 列只用于整体读写的不透明快照：数据库不按其内部字段查询、排序、关联或局部更新。
   每个 JSON 列对应唯一一个具名 Go 类型，并在列注释里写明（`包.类型`；store 不能 import 的包只写在注释里）；
   不重复其他列已有的事实；形状变更同样要迁移或读时兼容，并带旧数据测试。禁止 `map[string]any` / `Extra` /
   `Meta` 式的通用口袋，禁止一列多形态。非 JSON 的 text 列在注释里写明是自由文本或密文。
3. **值还是关系，是 JSON 与关系表的分界。** 一列里存的是所属行自己的一个*值*、由这一行整体读取和整体替换的，
   可以是 JSON：spec 的权重来源（`inference_specs.weight_sources`）是仓库名、revision 这样的描述性数据，
   随 spec 一起写入、一起读出。一列里存的是*实体之间的关系*、别的实体要经由它被查找的，属于关系：
   部署的节点列表（`deployments.node_ids`）存的是 `nodes` 行的 id，驱逐检查、网关选路、调度都要回答
   「哪些部署用到了这个节点」。判断时看 JSON 里装的是不是其他行的标识：是，就是关系。
4. **偶尔需要反查一个值时，在领域包里用归属类型解码，不在 SQL 里看进 JSON。** 从权重仓库找到声明它的目录模型
   由 `inferencecatalog.ModelDeclaringWeightRepo` 完成：store 只读出每个 spec 的所属模型 id 与 `weight_sources`
   两列，领域包用 `inferencecatalog.DecodeWeightSources`（该列唯一的解码入口）解码比较，命中后再加载那一个模型。
   这与 `monitor.rules`「store 只暴露原始 JSON，解析在领域包」是同一模式。
   代价是明的：在内置目录（379 个模型、1099 个 spec）上，这一次反查约 8 ms，它取代的 SQL 文本匹配约 1 ms；
   反查不能加载 spec 的其余列（整目录加载约 42 ms），也不能在循环里重复而不自知——它在部署预览中按候选节点各跑一次。
   若成为瓶颈，由调用方在一次请求内只解析一次，而不是退回到 SQL 里看 JSON。
5. **两个不加结构的判例。**
   - `weight_sources` 不拆子表。ADR 0009 决策 7 定的是「models + specs 两张表，列表筛选走 SQL」，spec 的 JSON
     字段对 SQL 不透明是有意的；加载 spec 的各处都整体读它，唯一看进它的是上面那个反查，一次扫一千来行、只读两列。
     为它拆第三张表要改全部预加载与测试字面量，换不来任何新能力。
   - `instance_events.data` 不把 `state` / `phase` 提成列。时间线折叠用 `json_extract` 取这两个字段，但新行由构造
     保证相邻两行的 state 或 phase 必不相同（只在二者变化时才追加事件），折叠只对旧版本写下的进度型历史行起作用，
     这些行由 retention 淘汰。为一个过渡性的读侧变换永久加两列，正是第 1 条要拦的事。
6. **由三条棘轮预算守护**（`scripts/arch-constraints.md`）：`store_text_columns` 限制能装 JSON 的列总数
   （`type:text` 列，加上注释声明 JSON 的定长列），新增必须上调并在 `# raised:` 里写明为什么不能是带类型的列；
   `store_text_columns_undocumented` 要求这些列都有注释；`store_sql_json_lines` 限制迁移之外看进 JSON 的 SQL
   行数（JSON 函数、`instr(`、`->` / `->>` 运算符，注释行不计），当前唯一的例外是上面的时间线折叠。
   定长列装 JSON 而注释不说、对 JSON 列做 `LIKE` 文本匹配，脚本都看不见，归评审。

## 后果

- `deployments.node_ids` 按第 3 条属于关系，现在仍是 JSON。它的解码收敛到 `store.Deployment.StoredNodeIDs`
  （`NodeIDList` 在其上回退到主节点），各调用方对空值与坏数据的既有处理不变；是否改成关联表待决策（`todo.md`）。
- `instances.schedule_expl` 除了给人看的放置解释，还承载后续决策依赖的启动事实（参数网络、PD 端点、unit），
  迁移曾用 `json_set` 改写其内部字段。它是否拆出这些事实待决策（`todo.md`）。
- 历史事件行淘汰后，时间线折叠 SQL 与 `store_sql_json_lines` 的这一个例外一起删除（`todo.md`）。
- 新增一个持久化事实时，评审先问能否推导、再问能否是带类型的列；选 JSON 时注释写清归属类型，预算的上调理由
  会出现在 diff 里。
- 本规则适用于每一个新增或改动的列。早于本规则、不满足第 2 条的已有列列在 `todo.md`，在那里处理之前保持原样；
  预算拦的是新增，不为已有的列背书。
