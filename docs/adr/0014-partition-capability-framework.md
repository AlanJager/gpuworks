# ADR 0014: 切分能力 ready 等于容器级探针通过

日期: 2026-08-24 · 状态: 已接受（框架范围见 `plan-partition-rebase.md`，探针决议见 `plan-partition-probe.md`）

## 背景

旧设计同时在节点和 GPU 上保存单值 `partition_mode`，并由厂商、MIG、agent 探测结果和前端判断分别推断“是否能切分”。七轴框架和资产上报消除了多数平行事实，但 mirror1 的 driver-only 910B4 事故证明，仅靠版本表与文件存在仍会产生假阴性：主机没有 CANN toolkit 时版本为空，server 在执行资产检查前就拒绝了节点；agent 的资产目录筛选还会再次按版本排除。两道先验门与“容器内提供 CANN”的真实部署形态冲突。

对标 gpustack-operator 后确认，版本组合表表达的是“为哪些组合构建过库”，不是运行时可用性的证明。mirror1 上的最小探针镜像实测约 2.5MB、端到端约 2.0–2.1 秒，且能通过 `dsmi_get_hbm_info` 精确观察 4096MiB/2048MiB 配额，因此用真实执行替代先验推断的成本可接受。

备选方案：A) 继续把版本组合表作为硬门；B) 只做 `dlopen` 加载检查；C) agent 上报资产后由 server 推断；D) agent 用真实注入合同运行一次性容器探针，server 只在探针通过后派生 ready。

## 决策

选 D：

1. **每张物理 GPU 的切分能力集合是唯一事实源**。节点能力只汇总 GPU 能力；调度、API 校验和前端展示读取同一集合，不再维护平行的 mode 字段。能力项只有 `ready` 与 `unavailable`，不可用原因由一个规则入口生成。
2. **agent 同时上报资产事实与行为探针结果**。资产清单仍不携带 ready；`PartitionProbes` 记录 backend、passed/failed/skipped、stderr 尾部和检测时间。server 的顺序为厂商匹配 → 容器 runtime → 完整资产 → probe passed → ready。旧 agent 未上报探针时明确提示升级。
3. **宣称与执行共享代码**。探针通过 `Engine.BuildPartitionProbeWorkload` 调用真实的设备注入与 `applyAscendPartition`/`applyPPUPartition`，再在内嵌离线镜像中读取 dsmi/HGML 配额视图；不存在独立的“探针版注入合同”。
4. **版本、CPU 架构、芯片族和 glibc 是兼容性提示，不是硬门**。主机版本为空不阻断；资产候选按版本评分但完整候选永不被排除。部署时另按所选镜像的 CANN 版本提示风险。
5. **切分后端采用七轴合同**：稳定 ID、厂商/架构匹配、成熟度、必需资产、注入合同、展示元数据、实例清理要求。注册表显式列出 NVIDIA、Ascend、PPU 后端，避免隐式初始化注册。
   PPU 首期执行路径固定为 Docker/runc，GPUWorks 显式注入 `/dev/alixpu*`；HGGC 13 来自经过目录精确匹配的业务镜像，不要求宿主安装专用 PPU container runtime，也不把尚未完成真机门禁的 udocker 路径标为 ready。极小探针镜像不内置厂商 SDK，只读挂载宿主驱动侧 `libhgml.so`/`libuki.so` 完成行为断言；宿主 HGGC 版本为空不能替代探针结论。
6. **准入故意不进入后端抽象**。GPU 健康、资源余量、镜像精确匹配、部署形态和产品策略由调度/API/镜像解析各自负责。健康状态与 capability 正交，因此 Warning GPU 可以报告 ready，但不会被调度。
7. **NVIDIA 本期维持既有行为**。内嵌 HAMi 资产存在即 ready，backend 维度的探针框架已预留；CUDA 侧行为断言另立后续决策，不阻塞国产探针改造。

## 后果

- 正面：ready 由目标节点真实容器执行证明，driver-only、空主机版本和未知芯片族不会再因先验表产生假阴性；失败保留可读的探针 detail。
- 正面：同一节点上的异构 GPU、同一厂商的多个候选后端以及 unavailable 原因都能被准确表达；实验性是成熟度而非绕过准入的开关。
- 负面/约束：agent 首次上线或资产变化会运行约 2 秒的一次性容器；device-share 是主机驱动状态变更，必须幂等且失败时给出修复命令。
- 负面/约束：探针证明的是内嵌探针镜像与当前节点的执行路径；用户所选推理镜像仍可能因 CANN/glibc 不同而失败，因此部署评价必须保留镜像级警告。
- 迁移成本：新增 `nodes.partition_probes`；旧 agent 未上报探针时能力 unavailable，升级后自动恢复。

## 拒绝的方案

- 扩展单值枚举：无法表达每 GPU 多后端、原因和成熟度，且继续制造多处推断。
- 继续使用版本硬门：driver-only 节点无法给出主机 CANN 版本，而且版本相符也不能证明容器注入链真的可用。
- 只做 `dlopen`：只能证明库可加载，不能证明 dsmi/HGML 配额视图、设备挂载、环境变量和 driver flag 的端到端语义。
- 把健康和资源准入放进后端：后端会依赖实时调度状态，无法成为稳定、可复用的执行合同。
- 以插件自注册替代显式注册表：隐藏初始化顺序和支持面，不利于穷举合同测试与安全审计。
