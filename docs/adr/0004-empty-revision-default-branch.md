# ADR 0004: 空 revision 表示跟随平台默认分支，不回填 main

日期：2026-08-23。状态：已接受。

## 背景

server 注册模型时曾不分 source 把空 revision 回填为 `main`。ModelScope 的默认分支习惯是 `master`，且其列文件 API 对不存在的 revision 返回 HTTP 200 + `Code:200` + `Files:null`（无错误信号），而 `/resolve/` 端点却容忍 `main`——导致"注册成功、下载必失败"且难以定位（mirror1 事故，见 `artifacts/archive/2026-08-23-model-download-revision/plan.md`）。

## 决策

空 revision 是一等语义：**跟随来源平台的默认分支，由平台解析**。全链路（api/store/proto/agent/pkg/modelrepo）允许 revision 为空；ModelScope 列文件不传 `Revision` 参数；下载 URL 用列文件响应中的 commit sha 钉住。UI 将 revision 收进高级选项。

## 备选与取舍

1. **按 source 硬编码默认**（modelscope→master、hf→main）：对默认分支非 master 的 ModelScope 仓库同样出错；把平台习惯固化进代码。
2. **agent 侧空列表时换分支名重试**：掩盖症状，多一次往返，且对第三种分支名仍失败。
3. **空 = 平台默认（采纳）**：唯一对所有仓库都正确的语义；代价是"下载了哪个版本"不再由注册时确定——用 commit sha 钉住补回确定性。

## 后果

- 存量 `modelscope + revision='main'` 数据需迁移置空。
- 下载的实际 commit 变成运行期事实，随状态上报、在 UI 高级信息展示。
- 测试夹具（fake hub）必须严格仿真平台对 revision 的真实行为，否则此类问题本地不可见（规约已入 CLAUDE.md）。
