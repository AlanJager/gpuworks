# ADR 0072: App 持有资源，每轮 Run 只停止与排空，Close 最终释放

日期：2026-11-18 · 状态：已接受

## 背景

同一 App 已允许在进程内停止后再次 Run，测试 harness 也依赖这个约定。但请求历史、长期统计和 trace exporter 在 New 时创建、固定绑定到 reader/sink，第一轮 Run 返回时却最终关闭。第二轮仍能推理成功，请求历史和服务质量查询却返回数据库已关闭。只重建某个字段不会更新已捕获旧对象的闭包。

## 决策

1. **App 与单轮 Run 有不同寿命。** New 构造并持有的存储、固定适配和 exporter 在 App 最终 Close 前保持有效；Run 拥有本轮监听、HTTP server 和后台循环。每轮 Run 结束必须停止收流、排空并写出最后记录，但不销毁下一轮继续使用的对象。
2. **Close 是最终释放入口，并且幂等。** 最终关闭后不能再 Run。Close 与 Run 的协调只有一份状态：关闭入口、停止并等待活跃运行结束，再按依赖逆序释放。等待时不得持有运行方完成所需的锁。
3. **调用方区分停止和销毁。** CLI 在退出时最终 Close；harness 的 StopServer 保留 App，最终 cleanup 才 Close。构造中途失败也释放已经构造的资源，不能只关闭主库而留下观测库、刷新循环或 exporter。
4. **排空顺序是行为契约。** 先结束请求及 agent 连接，再等待 accountant 的最后 enqueue，随后结束 metrics ingest 的最后 drain，并 flush 请求与统计。trace sender 属 App，跨轮保留，最终 Close 排空并释放。正常退出、强制 HTTP 退出和监听失败共用停止顺序，不能分别维护三份调用列表。
5. **Hub 负责自己的连接寿命。** 等待范围包含尚未认证的 ctrl/data 握手、server_dial 拨号及其子工作，而不只包含已登记 session。停止必须先阻止新工作进入并取消本轮连接，再等工作结束，避免关库后仍有握手或报告访问存储。

## 取舍

- 每轮重新打开并重绑整个请求管线，会让 sink、history reader、analytics 和 exporter 的引用重绑定扩散到装配各处；保留 App 级对象的现有身份，使重启只重建本轮资源。
- 只删除关闭 defer 会丢掉最终 flush/释放；只导出成功 session 的 Wait 也覆盖不到未认证握手。最终所有权和连接入场、退出都必须完整。
- 不引入通用资源依赖图或生命周期框架。同步状态仅表达运行/最终关闭与连接工作入场，已有业务算法与存储布局保持。

## 验证

同一 App 的两轮真实推理 HTTP 均成功，两轮记录都能经历史与质量接口查到，受控 collector 能收到 trace；末尾记录在停止时写出。覆盖最终 Close、重复 Close、Close 后 Run、并发停止、构造/监听失败和未认证握手退出。已有 accountant→metrics 的排空与循环退出回归继续有效。
