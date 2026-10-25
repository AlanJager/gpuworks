# MiniMax H3

MiniMax H3 是音视频生成模型。GPUWorks 可从 Playground 提交异步视频任务，并持续查询任务状态直至下载结果。

## 昇腾档位

当前昇腾档位经过 4 张 910B4 32 GiB 真机验证。模板会自动选择模型仓库中的 `FL2VA` 目录，保留仓库原有目录结构，并按 TP4、SP1 启动 SGLang Diffusion。用户不需要手工填写这些参数。

建议从 512 像素短边开始生成。实测 896×512、约 4 秒视频可完成，单卡峰值显存约 29,424 MiB。

当前档位不支持 910C/A3。上游 MiniMax H3 的 SGLang diffusion full-loop denoise 仍硬依赖 CUDA，替换 attention 后端后也会进入 CUDA JIT；把上下文改为 NPU 不能消除该限制。只有上游提供可用的 NPU denoise 路径并通过真机验证后，才能新增 910C 档位。

## NVIDIA 档位

NVIDIA 档位使用 GPUWorks 的 vLLM-Omni 0.30 runner（已包含 MiniMax H3 模块化管线），按 vLLM-Omni 官方 MiniMax H3 recipe 配置，每档固定卡数：

| 档位 | GPU 数 | 拓扑 |
|---|---:|---|
| 通用 | 1 | DiT 与文本编码器逐层 offload（容量路径，较慢） |
| Blackwell | 4 | Ulysses 4，不 offload |
| RTX 4090 | 2 | TP2 + 分布式逐层 offload，常驻 12 个 DiT 块 |
| RTX 5090 | 2 | TP2 + 分布式逐层 offload，常驻 20 个 DiT 块 |
| RTX PRO 5000 | 4 | TP2 × Ulysses 2，不 offload |

offload 档位把大部分权重放在主机内存：vLLM-Omni 要求至少 200 GiB 可用系统内存，建议 384 GiB。NVIDIA 档位尚未在 GPUWorks 上真机运行。

## 已知边界

- 当前档位仅接受 4 张单卡至少 32 GiB 的 910B4；其他卡数和型号不从一次真机结果外推。
- 768 像素短边在最终 VAE 解码阶段可能显存不足。
- 8 卡 TP8 会在权重分片前产生较高主机内存压力，当前不作为可用档位发布。
- 模板只下载并缓存运行所需的 `FL2VA` 仓库目录；如果节点已经缓存同 revision 的完整仓库，GPUWorks 会直接复用完整仓库。
