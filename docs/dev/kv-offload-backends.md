# 接入 KV 卸载后端

KV 卸载后端决定实例的 KV 缓存在 GPU 之外由谁保存：引擎自带的内存层与磁盘层，或引擎之外的组件
（例如 LMCache、Mooncake Store）。本页面向要接入新后端的贡献者，说明后端模型、各部分落在哪里、
新增一个后端要改什么。设计取舍见 [ADR 0069](../adr/0069-kv-offload-backends.md)。

## 后端模型

一个后端是 `internal/server/kvbackend` 里的一行描述，按 (引擎, id) 唯一，编译进二进制：

| 字段 | 含义 |
|---|---|
| `ID`、`Engine` | 后端 id 与所属引擎；id 就是部署参数 `kv_offload_backend` 的取值 |
| `Label` | 产品名，部署表单的卡片与实例面板原样展示 |
| `Default` | 引擎自带的卸载；`kv_offload_backend` 为空时解析到它，每个引擎至多一行 |
| `External` | KV 缓存由引擎之外的组件保存，请求能力快照记 `external_kv_cache` |
| `Vendors` | 加速卡厂商白名单，空表示不限 |
| `Replaces` | 选中时不可再设置的原生参数键 |
| `PDRoles` | PD 部署中保留该后端的角色，其他角色以无卸载启动 |
| `Topology` | 节点数上限与是否允许 PP、DP |

行只放用于判断的数据，没有 `init()` 注册，也没有包级可变状态。目前只有 `vllm/native` 一行，
因此部署表单与 API 的表现与只有原生卸载时完全相同。

## 各部分落在哪里

- **选择**：参数表中的 `kv_offload_backend` 是一个选择键（`Selector`），没有 flag，也不是平台参数。
  引擎有至少一个非默认行时才发布，选项由注册表生成；没有时请求里的该键被丢弃。
- **渲染**：`internal/server/deploy/kv_render_vllm.go` 按 id 提供每个后端的 connector 对象，
  `kv_compose.go` 的 `composeVLLMKVTransfer` 把 PD 片段与卸载片段合成唯一的 `--kv-transfer-config`。
- **校验**：`kv_offload.go` 返回下列码（文案在 `ui/src/i18n/locales/{en,zh}/common.json`）：

  | 码 | 何时返回 |
  |---|---|
  | `KV_OFFLOAD_BACKEND_UNKNOWN` | 引擎没有该 id 的可选后端；评估、编辑与启动都检查 |
  | `KV_OFFLOAD_BACKEND_CONFLICTS_NATIVE_TIER` | `Replaces` 中的原生键有有效值；平台按默认写入的值不算 |
  | `KV_OFFLOAD_BACKEND_CONFLICTS_ARG` | 非 PD 外部后端的参数层写了会覆盖其配置的 flag（`KVConflictFlags`，按拼写归一） |
  | `KV_OFFLOAD_BACKEND_UNSUPPORTED_ON_TARGET` | 目标厂商不在 `Vendors` 中 |
  | `KV_OFFLOAD_BACKEND_TOPOLOGY_UNSUPPORTED` | 节点数、PP 或 DP 超出 `Topology` |

  后两项在 `ValidateTargetFeatures` 中检查，两种校验模式都会经过，只收集能力时不检查。
- **PD**：`PDRoleLaunchConfig` 在角色不在 `PDRoles` 时清掉选择与原生字段。
- **反解**：引擎钩子 `RecognizeKVBackend` 把后端非 PD 渲染结果读回为选择，`ReverseArgs` 与
  `CanUnsetTemplateFlag` 共用它，另存为模板可以往返。
- **前端**：引擎画像的 `kv_backends` 列出可选后端（`ProfileForTarget` 按厂商过滤），部署表单每项一张卡片，
  选中时 `projectDraft` 取消该项 `replaces` 中的键。`ui/src` 不写后端 id。

## 新增一个后端

1. 在 `kvbackend.Builtin()` 加一行，只填它真正用到的字段；需要新字段时与第一个用到它的行一起加。
2. 在 `vllmOffloadRenderers()`（`kv_render_vllm.go`）加该 id 的渲染函数；若它的 connector 是纯卸载型，
   同时加入 `internal/engines/features.go` 的卸载 connector 列表，否则会被当成 PD。
3. 把 id 加进 `scripts/check_architecture.sh` 中 `kv_backend_id_literals` 的正则。
4. 测试：`kvbackend` 的表不变量；`deploy` 中 `TestKVBackendSetRendersEverySelectableRow` 会检查渲染函数与
   `Replaces`。通用规则已由测试行 `vllm/testext`（`kv_backends_helpers_test.go`）覆盖，新后端补自己的
   渲染结果与真机 L4 用例即可。
5. 在浏览器 E2E 台账 `test/e2e/coverage/deployment-wizard-create.json` 更新卡片的执行状态。

## 已知限制

- 混合厂商集群里调度器不按后端厂商筛节点，不支持的厂商在落位后的目标校验时被拒。
- 旧版本 server 会丢掉 `kv_offload_backend`：选了外部后端的部署在旧版本上以无卸载运行。
