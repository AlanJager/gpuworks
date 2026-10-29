# Kunlun P800 Qwen 模板包

包内仅增加 Qwen3-0.6B、Qwen3.8-27B 的 `bf16-vllm-kunlun-p800` 用户档位，沿用已有模型 ID，不替换 NVIDIA/Huawei 档位，也不修改内置镜像矩阵。完整英文契约见 [README](README.md)。

## 导入与单卡启动

1. 在仓库根目录执行 `python3 build/inference-templates/kunlun-p800/build.py --output /tmp/kunlun-p800-qwen.tgz`。
2. 将已审核的私有仓库镜像准备到所选 Docker 节点，按环境配置仓库登录。spec 直接锁定 manifest digest。`images.json` 仅是可选的旧式 tag 镜像目录条目，既不是内置镜像，也不是公开 runner 镜像；导入这些 spec 不依赖它。
3. Web 的“推理模板 → 导入模板”上传 tgz；在模型中选择上述 P800 档位。已有档位不会被删除。
4. 使用 Linux amd64、Docker 和 P800 PCIe/KL3 整卡，先保留包内默认参数做单卡验证。Qwen3.8 的此档位只开放文本。

镜像 tag 对应已记录的 K26 vLLM 0.25.1 的 manifest `sha256:b89a290ad64f62c24ca52923f509d6d906c7753b47c9d3dd4690003f58bc1aae`，含 Qwen3.8 适配；解释器固定 `/opt/vllm025-venv/bin/python`。正式公开镜像发布后再接入 upstream 内置目录，当前不声明不存在的公共镜像源。

权重为官方原版 BF16，revision 固定在 model.yaml；计算 dtype 为 `float16`。这是运行时类型转换，不是 AWQ/GPTQ/INT4 权重替换。权重大小与 KV 估算沿用 upstream 对应原版档位，不冒充新镜像的实测数据。

GPUWorks 单机设备接入只核对并注入所选 XPU 和控制设备，不要求 XDR、RDMA 设备、IPC_LOCK 或 memlock 放宽。单机多卡的镜像与卡间通信组合仍须单独验证；这不等于承诺任意镜像都能运行。

## 单机 TP

nnodes=1、PP=1，TP 为本节点所选卡数（1/2/4/8）。当前阶段不开放跨节点启动。默认 context 2048、单请求、批量 256 token、eager，面向先跑通。

## 验证边界

包测试覆盖导入、镜像匹配、官方 revision、卡型限制、设备逻辑编号、单机 TP、解释器、dtype 及拒绝路径。此前 P800 实测是配方的依据；新包导入成功不等于重新完成真机推理验收，精确版本与实测范围见交付报告。

通用的 Web 评估默认值继承修复独立于硬件支持交付。旧服务器上启动前显式选择 Float16，并核对命令预览，防止表单默认 auto 覆盖模板值。

spec 已锁定审核过的 manifest digest。可选镜像目录条目仍使用可变 tag，因为该 API 只接受 tag；不得将其当作不可变或内置镜像。
