# 远端运行时验收的已知缺口

本记录区分远端发行实例与本地主线 UI 的执行结果，不复制实时覆盖率。
对应规范：[浏览器 E2E 测试规范](../browser-e2e-testing-standard.md)。

## 远端一次性令牌弹窗残留明文

- **受影响构建**：远端 `dev-bfcf32c8` 内嵌 UI。
- **合同**：`gpuworks/inference-key/real-gpu-generation-revocation`。
- **状态**：远端内嵌 UI 的完整用例为 `BLOCKED_PRODUCT`。
- **实际证据**：浏览器创建推理令牌后，复制内容与接口响应及一次性展示值一致。
  点击「I have saved it」关闭弹窗，弹窗已不可见，但完整令牌仍存在于隐藏 DOM。
  `document.documentElement.outerHTML` 不含该令牌的条件等待在 30 秒后失败。
  断言只返回布尔结果，敏感阶段未记录 trace、截图或明文。
- **本地主线状态**：`ui/src/pages/apikeys/SecretModal.tsx` 已使用
  `destroyOnHidden`，弹窗关闭后销毁一次性秘密的内容。
  该修复尚不能由本地主线源码存在来证明远端旧构建已经生效。
- **修复验收**：远端安装包含该修复的 UI 后，重新运行完整用例，证明关闭后秘密
  不再留在 DOM；之后继续刷新脱敏回读、真实 GPU 流式/非流式生成及吊销闭环。
- **禁止绕过**：不得先刷新清除 DOM、删除秘密残留断言、注入页面脚本清空内容，
  或把只完成创建/复制标成完整通过。

远端旧构建的 Typography 复制按钮可访问名称为 `Copy`；本地主线向该组件传入
本地化 `secret.copy` tooltip，当前组件将其用作 ARIA 名称（英文 `Copy API token`）。
测试使用主线规范化后的可访问合同，不维护备用定位器来静默适配两个产品版本。
远端旧构建也不支持本次观察中的 Enter 提交，测试使用实际主操作按钮；本用例不声明
键盘提交覆盖。

## 两种验收环境分别报告

| Lane | 浏览器实际加载的 UI | 控制面、agent 与模型执行 | 能证明的边界 |
|---|---|---|---|
| `remote-real-agent-gpu` | 远端实例内嵌 UI | 远端真实服务、数据库、agent、容器与 GPU | 指定远端安装版本的端到端行为 |
| `local-ui-remote-runtime` | 明确 `ui_base_url` 指定的本地主线 Vite UI | 真实代理至同一个远端后端；不 mock 管理或推理响应 | 本地主线 UI 与真实远端运行时的组合行为 |

第二条 lane 的成功不能覆盖第一条 lane 的 `BLOCKED_PRODUCT`，不能用本地 UI 通过
宣称远端发行实例已修复或已经通过。报告必须分别记录 UI 来源及构建身份、实际后端
身份、case 和当轮证据；私有地址、凭据与资源标识只进入受控运行产物。

## 单节点网络范围

单节点 Serving 用例验证真实推理经专属网络定义选择的直连转发，以及 UI 解绑后恢复
管理网络。其使用同一块真实网卡，不代表独立物理网络隔离、跨节点传输、NCCL/RDMA、
带宽测试或断网时禁止回退的负例已通过。这些缺口仍单独保留。

双节点通信另由 `gpuworks/network/two-node-management-tcp` 验证，合同见
[remote-two-node-network.json](remote-two-node-network.json)。它使用管理网的双向真实 TCP，
不改变上述单节点 Serving 用例的范围，也不补齐 RDMA/NCCL、链路故障或分布式推理证据。

## 双节点测试详情关闭后焦点丢失

- **受影响构建**：远端 `dev-bfcf32c8`；修复前主线的 PairDetail 也在 pair 清空时直接卸载 Modal。
- **合同**：`gpuworks/network/two-node-management-tcp`。
- **实际证据**：真实双向 TCP 结果、独立 GET、刷新和详情视口断言通过，但 Esc 关闭后原方向的
  PairTag 不再聚焦，完整浏览器用例失败；不能仅凭两方向 passed 宣称该用例通过。
- **本分支修复**：`NetworkTestResult.tsx` 将显示状态与所选 pair 分开，保留关闭生命周期，
  由 Modal 恢复触发器焦点，在 `afterClose` 清除详情内容。相同 RTL 在修复前失焦、修复后通过；
  主线 UI + 真实双节点浏览器闭环也已通过，含 Tab 焦点约束及两个方向的 Esc 焦点返回。
  远端未升级，仍保留产品 blocker，本地主线 UI 的通过不代表远端已修复。
- **测试修正**：节点搜索须先点击展开再输入；未展开时 input readonly 是组件正常状态，
  本轮已确认远端可以正常搜索，不登记为产品缺陷。
