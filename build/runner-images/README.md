# gpuworks-runner 自建镜像构建仓

矩阵定义见 [matrix.yaml](matrix.yaml)；计划与证据见 artifacts 私有仓库 [artifacts/work/2026-10-03-runner-images/plan.md](../../artifacts/work/2026-10-03-runner-images/plan.md)。
**署名与派生关系见 [NOTICE](NOTICE)**：本目录的构建配方部分派生自 Apache-2.0 的
`gpustack/runner`，许可副本在 [licenses/Apache-2.0.txt](licenses/Apache-2.0.txt)。

## 目录

- `matrix.yaml` — 目标矩阵（厂商 × 框架档 × 引擎 × 架构 × base × requires × 版次）
- `cuda/` `cann/` `dtk/` `hggc/` — 各后端 `Dockerfile.{vllm,sglang}`（ARG 驱动，base 由矩阵注入）
  另有 `cuda/Dockerfile.vllm-v100`：V100 / V100S（sm_70）专用线，装 1Cat-vLLM 发行 wheel，不以上游
  vllm 镜像为 base（上游预编译内核不含 sm_70）；矩阵项标 `catalog: false`，只构建、验证、发布，
  不进系统镜像目录，由 L4 用例按卡型显式引用
  构建机到不了 GitHub 时，把发行 wheel 放到本机 HTTP 目录（`<url>/v1.5.1/1cat_vllm-1.5.1-cp312-cp312-linux_x86_64.whl`），
  用构建机本地的 matrix 副本给该项加 `ONECAT_RELEASE_URL` 指过去；SHA-256 校验仍照常生效
- `shared/` — 构建与验证共用的脚本
  - `assert_requires.sh` — 「requires 的含义」的唯一实现：构建末尾与 verify 都用它
  - `apply_patches.sh` — 严格打补丁（先 dry-run、不给 fuzz、不留 .rej/.orig）
  - `check_clean_history.sh` — 查镜像 config/history 里有没有凭据形态字符串
  - `check_vendor_base.sh` — 查 base 里有没有公司内部二次封装的痕迹（ZStack/AIOS 标记）；
    **必须在 `prepare-base.sh` 压 history 之前跑**，压完就查不到了
  - `stage_base.py` — 把上游 base「注册」进本地 mirror，已有的层零传输（见下）
- `overlays/` — 模型 overlay 与离线审计脚本（装进 `/opt/gpuworks/overlays/` 供 spec 引用）
- `patches/vllm/`、`patches/vllm_omni/` — vLLM 与 vllm-omni 的源码补丁
  （两者逐文件说明见下面「patches/ 与 overlays/ 逐文件说明」）
- `build.sh` — 构建驱动 ｜ `verify.sh` — 成品验证 ｜ `push.sh` — registry 间搬运
  ｜ `publish.py` — 发布到公网三端（见下面「发布」）
- `prepare-base.sh` — 压掉厂商 base 的 history（凭据清洗，只在 base 这一层做）
- `base-mirrors.example` — 构建机本地 base 映射的样例（见下）

## 关键约定

1. **tag 不可变**。每项有 `revision`（r1、r2…），产出
   `<id>-<revision>` 作为权威 tag，另打一个裸 `<id>` 作人用别名。
   推理目录（seed catalog）只引用带版次的 tag。改了配方就 +1，不原地覆盖——
   节点上已有同名镜像时不会重拉，原地覆盖等于改了没生效。
2. **matrix 的 `base` 永远是可复现的上游来源**。厂商 registry 不可达或只给 tar
   时，用构建机本地的 `base-mirrors.local`（不进仓库）显式映射到 mirror：

   ```bash
   cp base-mirrors.example base-mirrors.local   # 然后按本机实际 mirror 填
   ```

3. **上游 base 不必重新下载**。gpu3 到 docker.io 各镜像源实测只有 0.1~0.6 MB/s
   （daocloud、1ms.run 都试过），一个 10 GB 的 cuda base 要拉 4 小时以上。但只要本地
   registry 里已经有过从同一 base 构建出来的成品镜像，**base 的每一层就已经作为 blob
   存在那里**（镜像的层是内容寻址的，上游 base 的前 N 层正是成品镜像的前 N 层）。`shared/stage_base.py` 用跨仓库
   blob mount 把它们链进目标仓库，只补一个约 40 KB 的 config，digest 与上游逐个校验：

   ```bash
   export https_proxy=... no_proxy=<本地 mirror>   # 小对象走代理取 docker.io
   python3 shared/stage_base.py vllm/vllm-openai:v0.30.0 \
       <mirror>/vllm/vllm-openai:v0.30.0 --insecure --mount-from gpuworks-runner
   ```

   任何一层既不在目标仓库、也 mount 不过来就报错退出——这时才需要 skopeo 真拉一次。
4. **声明了就必须有**。`requires` 里写了的组件（omni/mooncake/diffusion/
   audit/hcu_patches），构建末尾断言不过就不出镜像；也有「声明了就必须没有」的：
   昇腾 omni 档的 `omni_no_mooncake`（pip 包 `mooncake-transfer-engine-npu` 必须不在，
   见下面「昇腾 omni 档」）与 `omni_patched`（vllm_omni 补丁落地）；`verify.sh` 再按
   镜像自带的 `/opt/gpuworks/image.json` 复验一遍。不再有 `|| echo WARN` 这种
   「名字里有、实际没有」的档。
5. **补丁严格应用**。上下文不匹配即构建失败（理由见 `shared/apply_patches.sh`
   顶部：`--fuzz` 会让纯追加补丁插进函数体中间且返回 0）。
   cuda vllm 档统一 `EXPECT_ALL=1`；确实不适用于某档 base 的补丁，必须由矩阵的
   `PATCHES_NOT_APPLICABLE` 显式声明（不是靠 `EXPECT_ALL=0` 悄悄放过）。
6. **构建期配置不留给运行期**。pip/apt 镜像源是 `ARG` 并在构建后还原，不写进
   镜像 ENV。
7. **厂商 base 的 history 可能含凭据**。厂商 vanilla（asllm、sourcefind dtk）的 image
   history 里带内网明文凭据，而 history 随镜像分发到每个 registry。`matrix.yaml` 里标了
   `base_requires_clean_history: true` 的档，必须先用 `prepare-base.sh` 把 base 的
   history 压掉、再把 `base-mirrors.local` 指向清洗后的 ref；`build.sh` 在构建前会校验
   effective base，不干净直接拒绝，`verify.sh` 再对成品复验一遍。
   **在 base 这一层压，不要对每个最终镜像压**：实测 hggc 两档共享 asllm 全部层
   （sum 50.4 GB / union 25.4 GB），逐个压会把这 50% 的去重打碎成两份独立单层。

## 构建机准备（一次性）

```bash
docker buildx create --name gpuworks-builder --driver docker-container --use
docker run --privileged --rm tonistiigi/binfmt --install arm64   # 跨架构
docker login "<目标 registry 主机>"                               # 推送凭据
cp base-mirrors.example base-mirrors.local && vi base-mirrors.local
```

## patches/ 与 overlays/ 逐文件说明

补丁由 `shared/apply_patches.sh` 严格应用（dry-run + `-F0`，不留 `.rej`/`.orig`），
清单写进镜像内 `/opt/gpuworks/patches-vllm.json`、`/opt/gpuworks/patches-omni.json`，
`verify.sh` 与 `assert_requires.sh` 的 `omni_patched` 会复验。

| 补丁 | 作用 | 适用 |
| --- | --- | --- |
| `patches/vllm/001_wrong_dp_ray.patch` | 给 `vllm/utils/network_utils.py` 的 `get_open_port` 补上缺失的 `_next_port` 模块级变量（DP + Ray 下取端口会 `NameError`） | cuda vllm 档。**在 vllm 0.31 上已不适用**（上游重构了 `get_open_port`）：`cuda12.9-vllm0.31.0` 档在矩阵 args 里用 `PATCHES_NOT_APPLICABLE` 显式声明，清单记为 `001_wrong_dp_ray.patch:declared-not-applicable`；声明与事实不符（补丁其实能应用）同样让构建失败 |
| `patches/vllm/002_mooncake_prom_metrics.patch` | 给 mooncake KV 连接器补 Prometheus 指标导出（上游只有内部统计，平台监控读不到） | cuda vllm 档 |
| `patches/vllm/003_mooncake_store_rejected_pending_load.patch` | MultiConnector 选中别的连接器时，mooncake store 不应再发起 pending load（否则重复加载/报错） | cuda vllm 档；**vllm 0.30 起上游已修**，识别为 already-present |
| `patches/vllm_omni/001_omni_bypass_non_omni_loads.diff` | vllm-omni 在 import 期无条件替换 vllm 的 `EngineCoreOutput` 等内部类型；本补丁加一个 `--omni` 判断，没带该参数时不改 vllm 行为，这样 omni 镜像也能跑普通文本负载 | 所有 omni 档 |
| `patches/vllm_omni/002_ming_audio_dsp_npu_cpu_fallback.diff` | Ming-omni-tts 的 `audio_dsp.py` 在昇腾上两处失败：`torch.hann_window` 对 int64 调 `aclnnInplaceCos` 失败、`F.fold` 在 aivec 向量核报 507035。补丁把这两步改在 CPU 上做再搬回设备（张量只有一个音频 chunk，开销可忽略） | 昇腾 omni 档（r3 起） |

| overlay | 作用 |
| --- | --- |
| `overlays/audit/stack-audit.py` | **离线组件审计门**：汇总引擎/依赖版本与 `pip check` 结果写成 `/opt/gpuworks/components.json`，errors 非空即构建失败。派生自同事镜像（见 `NOTICE`）；r2 起加 `GPUWORKS_PIP_CHECK_BASELINE`——只放行"纯净 base 本来就有"的 `pip check` 行，基线之外新增的才算我们引入 |
| `overlays/audit/omni_diffusion_check.py` | omni/diffusion 栈的离线内容检查（模块可定位、版本符合声明），不等于 GPU 验收 |
| `overlays/audit/qwen3_tts_check.py`、`qwen3_tts_prepare.py` | Qwen3-TTS 小档位的离线检查与"从上游资产推导单设备配置" |
| `overlays/audit/patch_sdxl.py` | 对固定版本的 omni 源码应用有界的 SDXL VAE 修复；源码漂移就报错，不静默发布未打补丁的镜像 |
| `overlays/gpuworks-{diffusers,transformers,vision_remote}-runtime.tar.gz` | 推理目录里海光 callable 档引用的适配器（`gpuworks_serve.py` + 各自 adapter），由 spec 解包使用 |
| `overlays/vllm-omni-h3-48298030.tar.gz` | MiniMax-H3 的 vllm-omni 源码覆盖层（上游镜像早于 H3 管线合入），内容寻址，由 spec 校验 SHA-256 后只读挂载 |
| `dtk/files/vllm-hcu-apply-patches` | 海光 `pre_start_argv` 依赖的工具（厂商 `custom:*` 镜像里提取，见 `dtk/files/README.md`），构建时内置进 dtk 档 |

## 复现一个档（从零到成品）

```bash
# 0) 一次性：见下面「构建机准备」；厂商/不可达 base 另需 base-mirrors.local
cp base-mirrors.example base-mirrors.local        # 按本机 mirror 填

# 1) 看这一档会怎么构建（不动任何东西）
./build.sh --list
./build.sh --only cann9.1-910b-vllm-omni0.30.0 --dry-run

# 2) 上游 base 若不在本地 mirror：先注册（已有层零传输，失败再用 skopeo 真拉）
python3 shared/stage_base.py quay.io/ascend/vllm-omni:v0.30.0     <mirror>/ascend/vllm-omni:v0.30.0 --insecure --mount-from gpuworks-runner

# 3) 厂商 base（matrix 里 base_requires_clean_history: true）：先查内部封装痕迹，再压 history
bash shared/check_vendor_base.sh <base-ref>       # 必须在 prepare-base.sh 之前
./prepare-base.sh <base-ref> <clean-ref>          # 然后把 base-mirrors.local 指向 clean-ref

# 4) 构建并推内网（多架构）；构建末尾会跑 assert_requires.sh，不过就不出镜像
./build.sh --only cann9.1-910b-vllm-omni0.30.0

# 5) 对成品复验一遍（按镜像自带的 /opt/gpuworks/image.json）
./verify.sh registry.zstack.ai/gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3

# 6) 真机验证通过后，再发公网并把裸 tag 指过去
./publish.py cann9.1-910b-vllm-omni0.30.0-r3 swr --dry-run
```

真机验证该看什么、已经验到什么程度，见 [`artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md`](../../artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md)。

## 常用

```bash
./build.sh --list                                   # 看矩阵（含版次与 requires）
./build.sh --only cuda13.0-vllm0.30.0 --dry-run     # 只看会怎么构建
./build.sh --only dtk26.04-vllm0.18.1 --no-push     # 构建到本地 daemon
./verify.sh registry.zstack.ai/gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2
./build.sh --revision-only r2                       # 构建并推送全部 r2 档
./build.sh --only hggc13.0-vllm0.23.0 --squash      # 厂商 base 需压 history
./verify.sh --all                                   # 对 registry 里全部版次复验
./push.sh --dest quay.io/gpuworks/gpuworks-runner    # 搬到另一个 registry
```

`REGISTRY` 环境变量决定推送目标，默认 `registry.zstack.ai/gpuworks/gpuworks-runner`。

## 设计取舍（相对上游 runner）

1. **精简工具层**：不装编译链（cmake/gfortran/openmpi 等），全部用预编译 wheel；
   镜像更小、构建更快。上游那套重型工具链对 gpuworks 的使用场景非必需。
2. **合并策略**：sglang+diffusion 合一个镜像（diffusion extras 是纯增量，不压核心
   依赖、不在 import 期改引擎行为）；**vllm 与 omni 分档**，因为
   `vllm-omni` 会把 `transformers` 压到 `<5.15`、`accelerate` 钉到 `1.12.0`，
   合并等于把这个上界加在镜像里所有负载上，而推理目录里 nvidia 的 vllm spec
   是 517 普通 : 27 omni。omni 档另发 `<框架档>-vllm-omni<版本>`，与昇腾线命名一致；
   两档同一 base，同仓库内大层完全去重，多这个 tag 只多约 280 MB。
   不再分 diffusion/tts 变体。
3. **昇腾线不装 PyPI 的 vllm-omni**：实证会让引擎的 registry 检查子进程崩
   （`artifacts/work/2026-10-03-runner-images/evidence/r1-verify.md` §2.2/§2.4）；昇腾的 omni 负载走 `vllm-omni` 档
   （base 自带 NPU 原生构建）。
   **omni 档（r3 起）的两处处理**——这是 r2 在 .32（8×910B4，driver 26.1.1，真容器）上
   实测出的（证据 `artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md` §6h）：
   - **卸掉 base 自带的 `mooncake-transfer-engine-npu`**。它是 `import vllm_omni` /
     `vllm --help` 退出阶段 `corrupted size vs. prev_size` 堆损坏的根因（与 vllm_omni 同进程
     加载时，`mooncake/*.so`、`libcann_hixl.so`、`libllm_datadist.so`、`libibverbs` 触发；单独
     `import mooncake` 不崩），也让 vLLM 的模型架构内省子进程 SIGABRT，Ming-omni-tts 与
     Qwen3-TTS 起不来。`quay.io/ascend/vllm-omni` 从 v0.24.0 起带它，v0.23.0rc1 没有。omni 档因此
     `MOONCAKE` 置空、requires 去掉 `mooncake`、加 `omni_no_mooncake`。
   - **Ming-omni-tts 的 NPU 补丁**（`patches/vllm_omni/002`）：`audio_dsp.py` 里 `hann_window`
     （int64 的 `aclnnInplaceCos` 失败）与两处 `F.fold`（aivec 向量核 507035）回 CPU 算。
   - 注意 omni 在 base 里是 editable 安装（`/vllm-workspace/vllm-omni`），补丁目标按
     `find_spec('vllm_omni')` 定位，不是 site-packages。
   - 推荐基线：旧驱动（26.0.rc1）机队用 `cann9.0-910b-vllm-omni0.23.0rc1`（上游原件无 mooncake、
     CANN 9.0.0）；HDK ≥ 26.1.1 用 0.30.0 的 r3。v0.25.0 的 Ming 另有独立的 `lm_head` 缺陷，别用。
4. **引擎可执行文件自带 env source**（昇腾线）：gpuworks 的 spec 一旦声明
   `entrypoint` 就会覆盖镜像 ENTRYPOINT，base 里那条 `source set_env.sh` 的链会断。
   把 source 放进 `vllm`/`sglang` 自身，两种情形都成立（做法参照上游）。
5. **cu129 的 vllm 档显式替换 base 里的 torchcodec**（`cuda12.9-vllm0.31.0`，证据
   `artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md` §6e、§6e.1）。上游 `vllm/vllm-openai:v0.31.0-cu129` 的
   `torchcodec 0.17.0` 是 **CUDA 13 构建**（`libtorchcodec_core*.so` 依赖 `libnvrtc.so.13`，镜像里只有
   `.12`），`import torchcodec` 直接 `OSError`；而 `vllm serve` 的 CLI 阶段会
   `from torchcodec.decoders import AudioDecoder`，所以**任何硬件上都起不来**。成因：上游用
   `uv --extra-index-url https://download.pytorch.org/whl/cu129` 装依赖，cu129 索引的 torchcodec 只到
   0.16.0+cu129，PyPI 的 0.17.0（Linux 默认 CUDA 13）版本更高，被选中。
   **做法**：`cuda/Dockerfile.vllm` 在其它 pip 步骤之后用 `--no-deps` 从官方 cu129 索引装
   `torchcodec==0.16.0+cu129`（torchcodec 0.14~0.17 都只要求 `torch>=2.11`，vllm 要求 `>=0.14`），
   由矩阵的 `TORCHCODEC_SPEC` / `TORCHCODEC_INDEX_URL` 驱动，其它档不传就不执行。
   **pip_guard 的按档例外**：`torchcodec` 仍在守护名单里，不是全局放开；Dockerfile 必须用
   `pip_guard.py replaced <name> <version> <reason>` 显式声明，声明会断言「已装版本等于声明版本且与 base 不同」
   并把 `{from, to, reason}` 写进镜像里的 `/opt/gpuworks/base-replaced.json`；`check` 只放行
   「恰好是 base→声明版本」的变化，任何别的变化（含同一个包变成另一个版本）仍然失败，构建日志会打印
   `DECLARED REPLACEMENT torchcodec: 0.17.0 -> 0.16.0+cu129`。**不是空壳模块**：是官方 cu129 wheel，音频解码走真实路径。
   **门**：`assert_requires.sh` 的 `engine_import` 对 vllm 档断言 `import vllm.multimodal.media.audio`，并在
   torchcodec 已装时断言 `import torchcodec`（二者离线、无需 GPU 即可判定），这个缺陷再出现会让构建失败。
   `-cu129-ubuntu2404` 变体与 gpustack 的 cuda12.9-vllm 镜像的对照见证据文档 §6e.1。
6. **ENTRYPOINT 其余情况保持上游原样**：gpuworks 部署时按 spec 注入 argv。
7. **厂商 base 先清 history 再用**：厂商 vanilla（asllm、sourcefind dtk）的 image
   history 里带内网凭据，而 history 随镜像分发。用 `prepare-base.sh` 在 **base**
   这一层压一次（不是对每个最终镜像压——实测那样会把共享层打碎，hggc 两档
   sum 50.4 GB / union 25.4 GB），`build.sh` 构建前会校验。
8. **callable runtime 不烤进镜像**：推理目录的 spec 已用 sha256 钉住同一资产并由
   agent 只读挂载到同一路径，镜像内那份永远被盖住。

## 已知待办

- `dtk/files/vllm-hcu-apply-patches` 来自厂商 `custom:*` 镜像（见 `dtk/files/README.md`）。
- cuda13.4 档等上游出 stable 后补；cuda12.8 上游已无 base、cuda13.3 不存在，均不补。
- `cann9.1-*-vllm-omni0.30.0` 三档 r3：910b 已在 .32（driver 26.1.1）真机验证；a3（无 A3 硬件）与
  950（容器环境已无）待复验，见 `artifacts/work/2026-10-03-runner-images/evidence/r2-verify.md` §6h。

## 发布（内网 registry -> 公网）

`publish.py` 把 `registry.zstack.ai/gpuworks/gpuworks-runner:<tag>` 发布到 Docker Hub / quay / 华为 SWR。
仅标准库、Python 3.6 兼容（gpu3 可直接跑）。镜像 = 上游 base 全部层 + 几百 MB 薄层，
所以发布的关键是**别重传 base 层**：

| 目标 | 跨仓库 mount（实测） | 结论 |
| --- | --- | --- |
| Docker Hub | 201，跨 namespace 可用（源 `vllm/vllm-openai`） | base 在 Docker Hub 的 cuda 档只传薄层 |
| quay.io | 201，跨 namespace 可用（源 `ascend/vllm-ascend`） | base 在 quay 的 cann 档只传薄层；cuda 档 base 在 Docker Hub，无法 mount，需全量上传 |
| SWR | 仅 SWR 内已有的 blob 可 mount（同 namespace 201）；外部 base 层 202；blob 的 HEAD 是账号全局的 | 已推过的层自动跳过，首次需全量上传；SWR 会把 OCI index 改写为 manifest list（index digest 变，平台 manifest digest 不变） |

```bash
# 只报告：N 个 blob、其中 M 个可 mount、K 个需上传、预计上传量（不做任何写入）
./publish.py cuda13.0-sglang0.5.21-r2 dockerhub --dry-run

# 真实发布（凭据只在进程内读取；--env-file - 从 stdin 读，可不落盘）
ssh gpu3 'cd /var/tmp/s7-publish && https_proxy=http://dev1.mytrix.me:7890 \
  no_proxy=registry.zstack.ai,172.25.17.156,172.26.1.15 \
  python3 publish.py cuda13.0-sglang0.5.21-r2 swr --dest-tag zz-pilot-... --env-file -' < secrets/registries.env
```

流程：读源 index -> 逐层判定（目标已有 / 可 mount / 需上传）-> mount -> `skopeo copy --all
--preserve-digests`（skopeo 跳过目标已有 blob）-> 校验（index/平台 manifest digest 与全部 blob）。
skopeo 凭据走 0600 临时 authfile，结束即删；日志对凭据脱敏。SWR 自动加入 `no_proxy`（国内直连，
实测走代理并不更快也不更慢）。退出码：0 成功，2 参数/配置，3 源读取失败，4 目标认证失败，5 skopeo
重试后仍失败，6 发布后校验失败。`--dest-tag` 默认同源 tag；正式 `-r2` tag 须在真机验证通过后再发。

## 同步阿里云盘（离线归档，发布后必做）

离线部署文档（[`docs/how-to/deploy-offline.md`](../../docs/how-to/deploy-offline.md)）让用户从阿里云盘分享目录下载镜像归档。所以**每次新增 runner 档或升级 revision，在公网发布之后，都要把归档同步到云盘**，否则离线用户拿不到新档。云盘上每个系列只放当前版本。

- 位置：企业云盘 drive 101 的 `/gpuworks-images/<引擎>-<框架>/`，目录名就是文档里列出的 `vllm-cuda/`、`sglang-dtk/` 等。文件名是 `gpuworks-runner_<tag>_linux-<架构>.tar.gz`；多架构的档，每个架构各传一份。
- 导出：在构建机上从内网 registry 导出，归档里的引用要写成规范形式 `gpuworks/gpuworks-runner:<tag>`，用户导入后不用再打 tag：

  ```bash
  skopeo copy --override-arch amd64 \
    docker://registry.zstack.ai/gpuworks/gpuworks-runner:<tag> \
    docker-archive:/var/tmp/gpuworks-runner_<tag>_linux-amd64.tar:gpuworks/gpuworks-runner:<tag>
  pigz -9 /var/tmp/gpuworks-runner_<tag>_linux-amd64.tar
  ```

- 上传：用 `pds-upload.py`。凭据（`PDS_DOMAIN_ID`、`PDS_API_KEY`）在维护者本地的 secrets 里。这把 key **只能上传、改名，不能删除**，传错了没法撤回，所以：先 `--dry-run` 核对目标目录和文件名；脚本拒绝覆盖已存在的文件；不要上传测试文件。

  ```bash
  env $(grep -v '^#' pds-ent-apikey.env | xargs) ./pds-upload.py gpuworks-runner_<tag>_linux-amd64.tar.gz vllm-dtk --dry-run
  env $(grep -v '^#' pds-ent-apikey.env | xargs) ./pds-upload.py gpuworks-runner_<tag>_linux-amd64.tar.gz vllm-dtk
  ```

- 旧版本归档删不掉，需要有人在云盘网页上手动删除或移走，提交说明里写明这一项。
