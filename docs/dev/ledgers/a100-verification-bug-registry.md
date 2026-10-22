# A100 验证缺陷状态台账

随提交维护的活文档。记录 `model-inference-validation/.../2026-09-02--gpuworks-a100-validation/evidence/bugs.md` 中平台侧缺陷在 **`feat/a100-p0-remediation` 分支** 的真实状态（`feat/blueprint5-fix` 分支的少量早期修复已合入 `master`，其余本表引用的提交仍只存在于 `feat/a100-p0-remediation`）。bugs.md 里的 "fixed in the working tree" 指验证用旁工作树 `../gpuworks_blueprint5`，不等于本分支已修；本表以本分支源码为准。

状态取值：`fixed`（已落库并有测试）、`partial`（部分落库）、`planned`（在 `artifacts/work/2026-09-04-a100-verification/plan-a100-p0-remediation.md` 内有明确方案）、`backlog`（未来修复，暂无方案）、`upstream_blocked`（runner/上游边界）、`semantic`（语义质量问题，不改平台）。

最后核对：2026-09-04，提交 `4b3dad9`（A/B/C/D/E 本地实现与分层验收；mirror1 L2/L3、gpu3 UDocker 与 provisioning restart 回放已记录）。

## 1. 已修复（fixed / partial）

| 编号 | 问题 | 状态 | 提交 | 说明 |
|---|---|---|---|---|
| BUG-006 | runtime 资产 root-only，udocker 用户挂不上 | fixed | `d9de071` | `internal/agent/engine/runtime_mounts.go` 按运行用户准备挂载路径；安装脚本 `chmod 0711` 与 `GPUWORKS_RUNTIME_MOUNT_ROOT` |
| BUG-016 | NVSwitch 节点 CUDA 无法初始化仍被接入调度 | fixed | `d381837` | 接入时 `gpu.CUDARuntimeSmoke`；doctor 增 fabric manager / peer access / NCCL 诊断。待 L4 复测失败节点不进入可调度池 |
| BUG-021 | 停止的 UDocker 实例进程残留、与替换实例冲突 | fixed | `d381837` | `Stop` 等待退出，3s 后 SIGKILL；`Remove` 对 not found 幂等，其余错误上抛 |
| BUG-030 | 网关截断非流式大响应（WAV / base64） | fixed | `d381837` | tunnel 请求方向 EOF 只半关闭；proxy `FlushInterval: -1`；proxy_test 增 audio/wav 用例。待 L4 回放 >700KiB 二进制 |
| BUG-037 | 下载无容量准入、ENOSPC 后进度清零 | fixed | `d381837`, `4b4e7b9`, `837abd4`, `c213965`, `d2ae578` | 下载前空间检查；失败上报实际落盘字节；预检查和写入竞态产生的真实 `no space left on device` 均分类为 `insufficient_disk_space`；L3 小 tmpfs 真实写满回放通过；UI 保留可续传字节 |
| BUG-044 | agent 重启后递归 chown 与 ETXTBSY | fixed | `1376fd1`, `4b4e7b9`, `4b3dad9` | ETXTBSY 保留原子替换；UDocker 所有权按根/bin + `.gpuworks-owner` 标记一次性迁移，移除重复全仓库 chown；gpu3 provisioning restart harness 预置 62 个 rootfs 验证重启后不再增加 chown 调用 |
| BUG-005 | 离线 unpack 丢 `/etc/alternatives` 链接 | fixed | `dbdd1b5`, `c694f4e`, `554260c`, `e4e9edd` | native udocker 解包器按 tar entry 保留 symlink/hardlink，并正确处理 whiteout、opaque 及目录↔链接替换；native 失败才回退 CLI。L3 harness 用合成 TLS OCI registry + bundled udocker/proot 验证 CUDA、c++、python3 alternatives 链、hardlink、whiteout、base 复用与回收；同一 helper 已在 gpu3 Linux 实跑通过 |
| BUG-015 | planning 使用根盘总量而非模型盘可用空间 | fixed | `dc36585` | `NodeSnapshot` 读取 agent `PathUsage` 的 `models.available_bytes`；缺失或非法时按 unknown（0）处理，不再误用 `DiskTotalKB` |
| BUG-043 | 无按节点驱逐模型缓存的 API | fixed | `4b4e7b9,3c688bc,c69b670,92c3f26,2c21493,527a532` | 新增管理员 `DELETE /models/{id}/nodes/{uuid}`，agent 仅删除 models root 下解析路径，store 幂等并拒绝活动 deployment（含分布式 NodeIDs 参与节点）；409 用稳定 `MODEL_IN_USE` 列出 deployment IDs；L2 真实 server/agent 证明驱逐后重连不回补 |
| BUG-002 | peer 获取失败不回退公网 | fixed | `4b4e7b9` | 有公网源自动进入 download desired state；无源明确 `source_required` |
| BUG-007 | ModelScope `revision=main` 导致空列表 | fixed | `4b4e7b9` | 注册入口将 ModelScope `main` 归一为 `master` |
| BUG-001 | 模板静默丢弃用户镜像 | fixed | `4b4e7b9` | 保留显式镜像，由统一 catalog/GPU 兼容校验决定是否 400 |
| BUG-003 | udocker image digest 为空 | fixed | `4b4e7b9` | native manifest 计算稳定 sha256 image ID，inspect/pull 状态回传 |
| BUG-004 | evaluate 权重元数据为空 | fixed | `4b4e7b9` | 使用 spec weight_bytes 回退并回写模型记录 |
| BUG-028 | 组件权重未计入 planning | fixed | `4b4e7b9` | breakdown 增加 component_weight_bytes，scheduler 使用组件总量 |
| BUG-049 | 嵌套挂载 evaluate 放行 | fixed | `4b4e7b9`, `ca44582` | launch 合成前与 manifest import 均拒绝父/子重叠目标 |
| BUG-059 | 模板与请求 model_id 冲突被静默覆盖 | fixed | `4fab9cf` | 显式 model_id 与模板权重身份不一致时返回冲突错误 |
| BUG-014 | systemd unit 无挂载依赖 | fixed | `4b4e7b9` | unit 增加 RequiresMountsFor 与 ExecStartPre 可执行校验 |
| BUG-063 | 恢复上传无超时/校验 | fixed | `4b4e7b9`, `84eaa44`, `8abf200` | 分阶段文件写入、sha256 校验、原子替换、停滞清理；L2 真实 SSH transport 覆盖缺目录、停滞和 sha mismatch；同阶段连续失败两次进入 `recovery_blocked` |
| BUG-010 | 同 digest 每实例完整重新 unpack | fixed | `b87b488,d12e174` | native 按 manifest digest 缓存 immutable base，后续实例使用 reflink/clone（macOS 回退普通 cp） |
| BUG-038 | 失败/删除实例 rootfs 未回收 | fixed | `4b4e7b9` | Remove 对精确 container 目录做残留清理；state.sync 后 CleanupOrphans 对账并记录失败 |
| BUG-031 | Mistral 双布局重复计入运行时权重 | fixed | `b175446` | spec/migration/API/snapshot 增 `runtime_weight_bytes`，显存按 loader 选中布局、磁盘按整仓及 present/partial/absent 增量；内置 Ministral 8B/14B 使用验证证据中的精确 consolidated 字节 |
| BUG-033 | native Mistral 布局被 config.json 校验拒绝 | fixed | `b0ce7f3` | validator 接受根目录 params.json + tekken.json + consolidated.safetensors，并拒绝嵌套/混杂布局 |

## 2. 计划中（planned，方案见 `artifacts/work/2026-09-04-a100-verification/plan-a100-p0-remediation.md`）

| 链 | 编号 | 问题 | 计划步骤 |
|---|---|---|---|

## 3. 未来修复（backlog）

### 3.1 catalog 参数 / API 契约（已有最小修正证据，批量落库，对应 `artifacts/work/2026-09-04-a100-verification/plan-nvidia-a100-verification-fixes.md` 的「M4：已实证 catalog 修复批量落库（P1）」章节）

BUG-008、013、018、019、022、023、024、025、026、035、038（dtype 半）、039、046、047、050、051、053。共同特征：模板参数、示例请求或 API 模式错误，修正候选已在真机通过。落库前用当前 runner 重放一次最小契约。

### 3.2 catalog 源与 Hub 解析

BUG-011（缺 fallback Hub 源）、BUG-012（源仓库已不可解析）。

### 3.3 平台能力（对应 `artifacts/work/2026-09-04-a100-verification/plan-nvidia-a100-verification-fixes.md` 的「M5.1 GPUWorks 必须修的平台能力」章节）

- BUG-036：offline UDocker TP2 在 `ncclCommInitRank` 中止；需 NCCL smoke 与失败日志保留。
- BUG-041：显式 deploy-config 在 Omni runner wrapper 中丢失。
- BUG-048：spec 级 capability 无法收窄 model 级 kinds。
- BUG-060：Diffusers spec 零权重字节过 planning。
- BUG-062：BF16 候选实际按 FP8 执行，元数据契约不一致。

### 3.4 组件 / runner 联合交付（对应 `artifacts/work/2026-09-04-a100-verification/plan-nvidia-a100-verification-fixes.md` 的「M5.2 catalog + runner/component 联合交付」章节）

BUG-020、027、029、032、034、042、045、052、054、055。需要镜像、组件或 checkpoint 所有者配合，不是纯平台改动。

## 4. 不由平台修复

| 编号 | 分类 | 处置 |
|---|---|---|
| BUG-009 | semantic | PaddleOCR-VL-1.5 endpoint 正确、语义错误；runtime PASS，semantic WARN |
| BUG-017 | 待重放 | Qwen3 FP8 SGLang A100 Marlin gate；用当前 runner 重放后决定放宽 |
| BUG-040 | upstream_blocked | SGLang GME 多模态 embedding 崩溃 |
| BUG-056 | semantic | Qwen3-VL-30B 视觉非确定 / 视频幻觉 |
| BUG-057 / 058 | upstream_blocked | Qwen3-4B FP8 在 SGLang 与 vLLM 均 exit 1，不再同版本重试 |
| BUG-061 / 064 | upstream_blocked | vLLM 0.28.0 Gemma4 loader / HunyuanOCR CUDA-graph 签名 |

## 5. 维护规则

- 关闭一个 BUG：把行移到 §1，填提交号，并在原始 bugs.md 条目回写提交号与 `platform fix / catalog fix / runner upgrade / upstream_blocked / semantic` 之一。
- 新发现的 BUG 先进 §3，进入计划后移到 §2。
- 表中 `partial` 必须写明"已做 / 未做"，不能只写 partial。
