# HTTP API 规范

`/api/v1` 下的接口都遵守本文的约定。新增或修改接口时照此执行，`make check-api-conventions`
（`scripts/apiconv`）会对能从类型上读出来的部分做检查。逐个接口的说明见 [api.md](api.md)。

## 字段命名

- JSON 字段名一律 snake_case：`node_uuid`、`memory_total_bytes`、`time_ms`。Go 结构体字段必须带
  `json` tag，不能依赖字段名的默认编码（那样会得到 `TimeMS`、`Entries`）。
- map 的键如果是数据（标签名、环境变量名、指标 label），不受此限。
- 带单位的数值把单位写进字段名：`_bytes`、`_gib`、`_ms`、`_seconds`、`_pct`。
- 时间点用 `_at`（RFC 3339 字符串）或 `_ms`（Unix 毫秒）结尾。同一个接口内不混用两种写法表示同一类时间。

## 集合

返回一组同类对象的接口，响应是一个对象，元素放在 `items` 里，**不返回裸数组**：

```json
{"items": [{"uuid": "…", "name": "gpu3"}], "total": 1}
```

- `total` 是命中总数。不分页时等于 `items` 的长度；分页时是筛选后的全部命中数，不只是这一页。
- 分页（`page`/`page_size`）另带 `page`、`page_size`，对应 `dto.ListPage`。不分页对应 `dto.List`。
- 游标分页（事件时间线、日志检索）不一定能给出 `total`，可以只给 `items`，以及翻页所需的游标字段或说明。
- 和整个集合相关、但不属于某个元素的信息（汇总、筛选选项）作为 `items` 的兄弟字段，例如
  `GET /api/v1/gpus` 的 `overview`、`GET /api/v1/nodes?page=1` 的 `labels`。
- 空集合是 `"items": []`，不是 `null`，也不省略。
- 筛选与排序参数不论是否分页都生效，**不得被静默忽略**：接口不支持的筛选值回 400（`LIST_FILTER_INVALID`），
  而不是返回未筛选的全集。
- 判断标准是「这个接口返回的主体是不是一组同类对象」。对象自身属性里的数组（一条曲线的 `points`、
  一个节点的 `gpus`）是对象的一部分，不受此限。

## 不透明字段

响应中类型为 `any`、`map[string]any` 或 `json.RawMessage` 的字段，检查脚本看不到里面的字段名。
这类字段只用于原样透出的整体快照（例如部署的 `catalog_selection_snapshot`），并且快照本身也要遵守 snake_case。
新接口优先定义具名 DTO。现有数量由 `scripts/arch-constraints.txt` 的 `api_opaque_response_fields` 预算约束，只降不升。

## 错误

错误响应固定为 `{"error": "<英文说明>"}`。属于产品错误时另带稳定的 `code` 和可选的 `params`，
客户端据此按 locale 显示文字。定义见 `internal/server/api/errors.go`。

## 检查

`make check-api-conventions` 对 `internal/server/api` 做类型检查，找出每个 `writeJSON` / `writeSSE`
调用写出的值，并报告：

- `envelope`：写出的是切片或数组，应改为 `dto.List` / `dto.ListPage`；
- `snake_case`：可达的结构体字段、或原地写出的 map 字面量的键，编码后不是 snake_case。

它包含在 `make test-unit` 中。
