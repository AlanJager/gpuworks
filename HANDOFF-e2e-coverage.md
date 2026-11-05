# e2e 覆盖率工作交接

分支 `test/e2e-runnable-and-coverage-20261028`（基线 `wei/master`），187 个提交，已推送至 `8a08ace83`。
**未合并**，MR 由用户创建，目标 `wei/master`。

## 当前数字

```
TOTAL        960/2953   checklist 914/2906, enum 22/23, combination 24/24
completion queue        1992 个未完成项
```

| 模块 | 覆盖 | 缺口 |
|---|---|---|
| deploy | 176/665 | 489 |
| deployments | 77/381 | 304 |
| models | 112/411 | 299 |
| images | 85/347 | 262 |
| networks | 60/191 | 131 |
| storage | 76/196 | 120 |
| apikeys | 89/184 | 95 |
| settings | 86/164 | 78 |
| nodes | 38/114 | 76 |
| logs | 23/87 | 64 |
| evaluations | 12/57 | 45 |
| dashboard | 7/37 | 30 |
| **assistant / inference-templates / login / playground / providers / terminal** | **全部 100%** | **0** |

## 三关（在 `git archive HEAD` 的干净提取里验证通过）

```bash
# gate1 队列基线
PYTHONPATH=test/e2e python3 -B -m unittest test_coverage_completion_queue     # OK

# gate2 覆盖率脚本
PYTHONPATH=scripts:test/e2e PYTHONDONTWRITEBYTECODE=1 \
  python3 -B -m unittest discover -s scripts -p 'test_e2e_coverage*.py'       # 87 tests OK

# gate3 空白
git diff --check                                                              # clean
```

改任何 manifest 后，`test_coverage_completion_queue.py` 的 `BASELINE` dict 和
`assertEqual(total, N)` 必须从 `summary()` 实测同步，不要手算。同步方法：

```bash
rm -rf /tmp/gwx && mkdir -p /tmp/gwx && git archive HEAD | tar -x -C /tmp/gwx
cd /tmp/gwx && PYTHONPATH=test/e2e:scripts python3 -B -c \
  "import coverage_completion_queue as Q; e,c,t=Q.summary(); print(dict(sorted(c.items()))); print(t)"
```

## 本轮（2026-11-04/05）做了什么

### 覆盖 +5

| 位置 | 项 | 变化 | 依据 |
|---|---|---|---|
| `network-storage-lifecycle.json` | 12.7 | MISSING → covered | 级联解绑实测 + 无 in-use 能力 |
| `network-type-roles.json` | 12.7 | PARTIAL → covered | 该 case 自身证据 |
| `platform-token-options.json` | 12.7 | MISSING → covered | revoke 是硬删 |
| `instance-diagnostics.json` | 12.7 | MISSING → covered | 允许清理 + 容器拆除实测 |
| `nodes-labels-search.json` | 2.5 | MISSING → covered | 真实约束冲突 + 非空前置 |
| `http-model-registration.json` | 12.8 | MISSING → N/A | 分母 −1，分子不虚增 |

**12.7 这个「记账滞后」模式已清零**：全仓 37 条 MISSING/PARTIAL 里，使用该可疑模板的现为 0 条。

### 修掉 5 处恒真断言 + 1 个假 report 字段

`nodes_labels_search.py` 四处 `labels == {}`、`assistant_service_tools.py` 两处，以及后者的
`report["write_attempt"]["labels_unchanged"] = True`（字面量）。

**根因**：断言「被拒绝的操作没有改变状态」时，前置状态本身为空 → 空对空比较，永远不会失败。
**修法**：先写入种子值，**并额外断言种子生效**——否则种子静默失败会退化成同样的空对空比较。

## 方法论（这部分比数字重要）

### 1. 「空断言」的四种形态

| 形态 | 例子 | 信号 |
|---|---|---|
| 假 PASS | PASS 但 `10.x` 证据键全缺 | 报告里找不到证据键 |
| 恒真子串 | `"ant-pagination-prev" in class` 恒真 | 需要 token 切分比较 |
| 延迟求值 | `filter_kept: False` 但断言已通过 | 值在后续步骤改变后才计算 |
| **零对零比较** | `labels == {}`、`instances: 0 → 0` | **无任何信号** |

**第四种分两型**：
- **4a 未初始化**：fixture 没准备好，`0 → 0` 一眼可疑
- **4b 合法空**：`{}` 是这个 case 的正常状态，**读代码的人会认为正确**

4b 最危险，因为断言自身没有任何异常表现。

### 2. 四种区分，判定「未覆盖 vs 不适用」时用

| 情况 | 记法 |
|---|---|
| 产品没有这个能力 | N/A，附代码依据 |
| 代码有分支但不可达 | N/A，记录分支位置 |
| **我们的测试无法经济地构造** | **unknown，绝不写 N/A** |
| 产品有，但形状与清单措辞不同 | covered，**必须写明形状差异** |

**N/A 会缩小分母。** 用「我们的测试做不到」写 N/A 等于用没做的工作抬高百分比。

### 3. 「四个资源有、它没有」比 grep 零命中强

`internal/server/api/errors.go` 定义 `SERVED_NAME_IN_USE` / `MODEL_IN_USE` / `IMAGE_IN_USE` /
`STORAGE_IN_USE`，唯独没有 `NETWORK_IN_USE` / `TOKEN_IN_USE` / `DEPLOYMENT_IN_USE`。
**零命中可能只是没搜到；对照组说明设计上就不打算有。**

### 4. 相似文案不能合并覆盖

推理 key 的 revoke 保留记录、置 `revoked_at`、幂等
（`internal/server/api/apikey_validation_test.go:99-125`）。
平台 token 的 revoke 是硬删（`internal/server/store/platform_tokens.go:140`）。
**两者 UI 都叫 Revoke，文案只差一个分句。** 规范
`browser-e2e-component-checklists.md:14` 明确禁止按标题相似绑定。

### 5. manifest 声明文本里不能出现降级词

`_status()` 对整段文本做子串匹配，命中 `PARTIAL` / `MISSING` / `NOT_COVERED` 就降级。
**在解释「为什么保持 partial」的句子里写 `partial`，会让这条声明自己降级。**
JSON 合法、run PASS、证据齐全，**没有任何信号**。

### 6. 同源背书的主键是 `test_sha256`，不是 `binary_sha256`

`make build` 每次注入新的 BuildDate，**二进制哈希必然变化**。
只有当多次 run 复用同一构建产物时，`binary_sha256` 一致才有意义。

### 7. 「PASS」只表示没有断言失败，不表示断言执行了

每次都要确认新证据键**真的在 `report.json` 里**。

### 8. case 是有状态的

改了共享状态，后续所有步骤都受影响。已踩过两次：UI 控件状态（page size 跨 reload 持久）、
测试数据（改了 label 值破坏后续断言）。**探针用完必须恢复原值，并在注释里写明。**

## 运行 e2e 的前置

```bash
cd /home/test/gpuworks-e2e-coverage-80-20261023
export TMPDIR=/home/test/gw-e2e-tmp NO_PROXY=127.0.0.1,localhost
```

- **移植性**：`test/e2e/local_browser_fixture.py:144` 用 `range(8800, 8900, 2)` 探端口，存在
  TOCTOU 竞争。症状 `bind: address already in use` → 重试。
- **`http()` 用裸 urllib**（`:34-50` `build_opener(ProxyHandler({}))`），不是 `page.request`，
  所以不会污染 `page.on("response")` 的监听。
- **Go 或 UI 改动**：先 `cd ui && npm run build`，再重建 `bin/gpuworks`。
  `dist` 是 `go:embed all:dist`，**只改 UI 不重建二进制会静默 false-PASS**。
- **`deployment_health.py`** 需要 `DOCKER_HOST`，否则 `BLOCKED_INFRA`。可用值
  `unix:///tmp/gpuworks-e2e-gwks-fix-docker/docker.sock`。
- 单次 run 产物约 13M。

## 已知未做（有意为之，不是遗漏）

| 项 | 原因 |
|---|---|
| `10.8`（100 容器） | 规模成本过高 |
| `10.11`（flat ResourceTable） | 产品形状与清单不符 |
| `5.3`（`searchable_select`） | **`deployment_wizard_create.py:916` 的 `assert_searchable_select` 是死代码，零调用者**。5.3 已在 manifest 保持 MISSING，评分未被虚增。**下一个做 5.3 的人必须知道这个函数从未执行过。** |
| `2.6`（多行粘贴） | 未开始 |
| `storage-nfs-cache-policy` 12.7 | 夹具是 25 个 offline 节点，清缓存需在线节点 |
| `models` 12.5 / 12.8 | `RegisterModelModal.tsx` 只有一个 Modal，无嵌套弹层可测 |

## 遗留的操作性问题

- **磁盘 971G/983G，仅剩 2.3G。** `/tmp/gwks-test-scratch` 66G 属其他 lane（10/29-30），未动。
  大头在宿主侧，需从宿主排查。
- **有一次误删**：某 lane 按「大写 T + 非 PASS」批量删了 141 个 run 目录。已核实
  **全部 25 个被 commit message 引用的 run_id 完好**，损失仅为其他 lane 的 FAIL_TEST 现场。
- **未提交的未跟踪文件**（非本工作产物，不要提交）：`bin`、`.omx/`、`handoff.md`。
