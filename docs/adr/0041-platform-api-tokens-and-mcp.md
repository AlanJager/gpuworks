# ADR 0041: 平台 API 令牌与 MCP 服务共用一种凭证

日期：2026-10-11 · 状态：已接受

## 背景

管理 API 只认登录会话（JWT 或 Cookie，24 小时过期），脚本和外部 AI 工具没有长期可用、可吊销的凭证。已有的 API key（`gw-`）只用于 `/v1` 推理网关，不代表任何用户。
AI 助手的工具已改为以提问用户的身份读本平台 API（`aihelper.APIReader`），同一套工具适合通过 MCP 提供给 Claude Code、Cursor 等外部工具，但需要一种能代表用户的凭证。

## 决策

1. **新增平台 API 令牌，与推理 API key 分开**：明文 `gwp_` 开头（256 位随机），只存 SHA-256 与显示用前缀，独立表 `platform_tokens`。
   随机串熵足够，不用 argon2，每次请求一次索引查找。
2. **同一个令牌既调管理 API 也调 MCP**，不另设 MCP 专用凭证。
3. **令牌代表创建者**：每次请求按用户表的当前角色鉴权，再叠加令牌范围 `read`（GET/HEAD 与 `POST /api/v1/mcp`）或 `write`（与登录相同）。默认 `read`。
4. **任何令牌都不能管理令牌**（`/api/v1/platform-tokens` 只接受登录会话），防止只读令牌给自己换读写令牌。
5. **鉴权在外层 `managementAuth` 一次解析**，把身份放进 request context；`claimsFromRequest` 先读 context，各处调用与 `managementAdmin` 不变。
   令牌范围由路由声明，不在中间件里按路径判断：令牌管理接口挂 `sessionOnly`，其余管理路由挂 `readTokensRead`，`POST /api/v1/mcp` 不挂（工具只读）。
6. **MCP 用 Streamable HTTP，只返回 JSON**：协议在叶子包 `internal/server/mcp`，不依赖本仓库任何包；工具即 `aihelper.BuiltinTools()`，
   与 agent 循环共用 `Tool.Run`（同一份报错与截断），经 `APIReader` 以调用者身份读 API，只读，与令牌范围无关。管理员开关，默认关闭。
7. **校验结果短时缓存**（1 分钟，按令牌哈希）：带令牌的请求原本每次读令牌和用户两行，MCP 的每次读取还要再校验一遍。经本 store 吊销立即失效，
   与吊销并发的读取不会把已吊销的令牌放回缓存。

## 取舍

- 不复用 `api_keys` 表加一列「用途」：推理 key 不属于用户、走 argon2 加缓存的网关热路径，管理令牌要绑定用户与范围；混在一起两边的语义和性能取舍都要妥协。
- 不做细粒度权限（按资源或按接口授权）：平台只有 admin / user 两种角色，两级范围已覆盖「给 AI 工具只读」与「给自动化脚本读写」两种用法；需要时可在 `scope` 上扩展。
- 不开 SSE 流：工具都是一次性读取，JSON 响应足够，也不必维护会话。
- 吊销即删除，不保留吊销记录；审计靠创建、吊销与 MCP 工具调用的 slog。

## 后果

- 新表与迁移 137；设置键 `mcp.enabled`；`api` 多依赖一个包（`mcp`，架构预算 `api_fanout` 39→40）。
- 文档：[通过 MCP 接入](../how-to/use-mcp.md)、[API 参考：Platform Tokens](../reference/api.md#platform-tokens平台-api-令牌)。
