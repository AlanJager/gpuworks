# 贡献指南

## 开发流程

1. 从目标分支创建范围明确的分支；不要混入无关改动。
2. 先从代码、测试和当前 ADR 确认契约，再修改实现与同层测试。
3. 单文件超过 600 行时拆分；DTO 与 store entity 分离，事务提交后显式发布事件。
4. 运行 [`CLAUDE.md`](../../CLAUDE.md) 要求的提交前检查，再提交 MR，并写明未覆盖分支与外部验收状态。

## 文档落位

- 教程放 `docs/tutorials/`：从零学习主线，不展开原理。
- 操作指南放 `docs/how-to/`：解决一个任务，不重复入门教学。
- 精确字段、矩阵和协议放 `docs/reference/`：不写叙事式操作流程。
- 设计动机放 `docs/explanation/`；不可逆取舍另写 `docs/adr/NNNN-*.md`。
- 开发操作放 `docs/dev/`；内部活台账放 `docs/dev/ledgers/`。
- 一次性计划、验收、审计和真机过程放 `artifacts/`：进行中放 `work/<YYYY-MM-DD>-<主题>/`，收口后
  整个主题目录搬进 `archive/<YYYY-MM-DD>-<主题>/`，独立完成的调研放 `research/<YYYY-MM-DD>-<主题>/`；
  状态只写在计划文件自身的 YAML frontmatter 里，索引见 `artifacts/README.md`。

`docs/` 全部使用中文，代码、API 字段和 CLI 输出保持原文。现行正文不绑定 M 编号，不依赖
仓库外相对路径；需要历史证据时只链接仓库内 artifact。根目录
[`CONTEXT.md`](../../CONTEXT.md) 是术语的唯一完整定义，用户术语表只做子集。

## 写作约定

- how-to 和教程的标题下紧跟加粗的「适用场景」「前置条件」两行（教程用“开始之前”表格），新手一眼判断是否该读。
- 截图放 `docs/assets/screenshots/`，用 `![说明](../assets/screenshots/xxx.png)` 引用；界面用中文，分辨率 1440×900 起，避免出现真实地址、令牌和主机名。发布站点由 `docs-site/stage_docs.py` 负责把图片复制并改写成站内路径。
- 流程图用 ```mermaid 代码块（GitHub 与发布站点都能渲染），节点标签含括号、冒号时用双引号包裹。
- 链接检查：`make check-doc-links`（已包含在 `make test-unit`）校验相对链接和 `#锚点`，并拒绝 `docs/` 下的 `[[wiki]]` 引用。
- 描述代码里的具体行为（错误码、flag、默认值、端口）时先读代码再写；参考手册里的 flag 表以 `--help` 输出为准。

## ADR

新增 ADR 前先确认问题确实是长期架构取舍。编号取 `docs/adr/` 当前最大值加一，写明日期、
状态、背景、决策、后果和被取代关系，并更新 `docs/adr/README.md`。实现文档链接 ADR，
不要在多个指南中复制决策全文。

