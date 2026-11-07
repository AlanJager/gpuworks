# Kimi-K3.1-Pro

Kimi-K3.1-Pro keeps the Kimi-K3 layout (Kimi Delta Attention, 896 routed experts with 16 active per token, a vision encoder) and adds sparse attention with a top-2048 indexer to 24 of its MLA layers. The checkpoint ships MXFP4 routed-expert weights (about 1.56 TB on disk); everything else is BF16. Context is 1M tokens.

Both engines need the day-0 images published with the model; the platform runners and older upstream images do not load it:

- **SGLang**, from the [SGLang cookbook](https://docs.sglang.io/cookbook/autoregressive/Moonshotai/Kimi-K3.1-Pro): `lmsysorg/sglang:dev-kimi-k31-pro` (NVIDIA) and `lmsysorg/sglang:dev-kimi-k31-pro-mi35x` (MI350X / MI355X). SGLang always keeps the KV cache in FP8.
- **vLLM**, from the [vLLM recipe](https://recipes.vllm.ai/moonshotai/Kimi-K3.1-Pro): `vllm/vllm-openai:kimi-k31` (NVIDIA, CUDA 13) and `vllm/vllm-openai-rocm:kimi-k31` (ROCm 10), which need vLLM 0.34.0 or later; the PPU profiles use the vendor's vLLM 0.32.0 build `egslingjun-registry.cn-wulanchabu.cr.aliyuncs.com/egslingjun-tmp/inference-xpu-pytorch:kimi-k3.1-pro-vllm0.32.0-cuda13.0-pytorch2.13.0-20261103`.

## Profiles

| Profile | Hardware | Notes |
| --- | --- | --- |
| `mxfp4-vllm-nvidia-b300-tp8` | 8 × B300 | Recipe default TP8, **verified on hardware** (below) |
| `mxfp4-vllm-nvidia-b300-tep8` | 8 × B300 | Recipe TEP: expert parallelism on MegaMoE, lowest long-prompt TTFT |
| `mxfp4-vllm-nvidia-b300-tp8-dcp8` | 8 × B300 | Recipe decode context parallelism: about 7× the KV tokens of TP8, decode 7–25% slower |
| `mxfp4-vllm-nvidia-gb300-tp8-multinode` | 2 GB300 trays (2 × 4) | Recipe default TP8 across two trays |
| `mxfp4-vllm-amd-mi355x-tp8` | 8 × MI355X | Recipe TP8 with the AMD AITER environment |
| `mxfp4-vllm-ppu-810e-tp32-ep-multinode` | 2 × 16 PPU 810E (TP32 · EP32) | Recipe multi-node TEP; the W4A16 MoE path `ppu_deep_gemm_w4a16` |
| `mxfp4-vllm-ppu-m890-tp8-dp-multinode` | 4 × 8 PPU M890 (TP8 × DP4 · EP32) | Recipe multi-node TEP: one TP8 data-parallel replica per node, DeepEP low-latency all-to-all |
| `mxfp4-sglang-nvidia-b200-tp-dcp-multinode` | 2 × 8 B200 (TP16 · DCP16) | Cookbook "Balanced" command; DCP follows the deployment's TP |
| `mxfp4-sglang-nvidia-b300-tp8` | 8 × B300 | Cookbook low-latency TP8 |
| `mxfp4-sglang-nvidia-b300-tp8-dcp8` | 8 × B300 | Cookbook high-concurrency TP8 · DCP8 · EP2 |
| `mxfp4-sglang-amd-mi355x-tp8` | 8 × MI350X / MI355X | Cookbook low-latency TP8 on the AMD monokernel, 1–4 concurrent requests |
| `mxfp4-sglang-amd-mi355x-tp8-dcp8` | 8 × MI350X / MI355X | Cookbook high-throughput TP8 · DCP8, up to 48 concurrent requests |

The NVIDIA and AMD vLLM profiles add two settings the recipe leaves out, both found on hardware: `--max-model-len 262144` (one 1M-token request needs 16.92 GiB of KV per card, more than a 275 GB B300 has left after 195 GiB of weights) and `--max-num-seqs 128` (every decoding sequence takes one Kimi Delta Attention state block; vLLM's default of 1024 exceeds the 839 blocks that fit, and the engine refuses to start). Raise the context in the deploy form if a card has the room; vLLM estimated about 876K tokens on a 275 GB B300.

Multi-node profiles need a shared compute network between the nodes; see multi-node inference in the platform documentation. Set TP to the total GPU count (16 for two B200 nodes, 32 for two 810E nodes, 8 for two GB300 trays). The M890 profile runs TP8 on exactly 8 cards per node with one data-parallel replica per node: deploy it on four nodes with TP 32 and data parallel 4 (the node count), and the engine runs TP8 × DP4. Without data parallel the launch is refused before start for a GPU-count mismatch.

The PPU profiles leave out three settings of the upstream recipe: `NCCL_SOCKET_IFNAME=eth0` and `NCCL_IB_HCA=fic2,vsolar` name the recipe host's interfaces, which the platform sets from the compute network instead, and `--max-model-len auto` is not accepted by the platform's parameter dictionary; without it vLLM uses the full 1M context, whose KV cache fits in 32 PPUs.

## Measured

`mxfp4-vllm-nvidia-b300-tp8` on 8 × B300 SXM6 (275 GB per card), weights on shared storage:

- Weights load in 189 s cold and 107 s from a warm page cache, 195.04 GiB per card; CUDA graphs take 53 s and 5.71 GiB; the KV pool is 33.51 GiB per card, 1,972,777 tokens (7.53 concurrent 262K-token requests). The first start also spends about 23 minutes in flashinfer's MoE autotuning, so set the deployment's start timeout to 3600 s.
- Chat and reasoning parsing (`reasoning_content`) work; tool calls were not exercised.
- Platform benchmarks, every request successful: random 2048/512 (3,180 input tokens counted by the server) reached 1,473 output tok/s and 10,624 total tok/s at concurrency 64; agentic 80K (13 turns, about 119K input tokens) 225 output tok/s at concurrency 8; agentic 200K (about 203K input tokens) 137 output tok/s and 127,066 total tok/s at concurrency 4, with 98% prefix-cache hits on later turns.

Kimi's tokenizer is tiktoken (`tiktoken.model`), so a benchmark's automatic tokenizer finds no `tokenizer.json`; converting it with transformers' `TikTokenConverter` gives identical token ids and can be imported as the benchmark tokenizer.

## 910C (A3) · W4A8

The `w4a8-vllm-ascend-a3-multinode-16npu` profile uses the official `kimi-k3.1-pro-a3` image and W4A8 checkpoint (about 1.47 TB on disk and 1.34 TiB in device memory). It runs one co-located multi-node instance: TP16 within a node and one pipeline stage per node. Two nodes are the fewest that hold the weights; four match upstream's Prefill stage count. Cache dtypes, block size, parsers and MLAPO come from upstream's Decode role; DCP, PCP and the interleave size belong to the separated roles and are left out. The co-located profile does not enable the Decode-only `recompute_scheduler_enable` flag.

This configuration has been validated on two 910C nodes with 16 dies each, using TP16 per node and PP2 across the nodes: `--max-model-len 210000`, `--max-num-seqs 64`, and `--gpu-memory-utilization 0.90`. All 836 requests passed across the random 2048/512, Agentic 80K and Agentic 200K matrices, with another 93 requests covering multiple input lengths, real multi-turn conversations, Agentic 10K and a single 200K request. Reasoning and tool calling require `--tokenizer-mode`, both parsers and `--enable-auto-tool-choice`, which the profile configures. Ascend multi-node runs on an accelerator RDMA or HCCS network, so a deployment needs one covering every node it spans. No 950DT profile is stated — 8 devices per node cannot hold these weights, and no 950DT hardware was available.

## Not supported

- **Hopper (H100 / H200)**: with the current images, SGLang runs out of memory while loading (the routed experts are not loaded as MXFP4 on sm90), and vLLM stalls in the Triton fallback of the `attn_res` kernel, which has a native implementation only on Blackwell. Upstream marks these commands as unverified; the profile returns once an image runs on Hopper.
- Not included: speculative decoding (SGLang DFLASH with `RadixArk/Kimi-K3.1-Dflash2-Preview`, vLLM DSpark with `Inferact/Kimi-K3.1-Pro-DSpark-preview`), HiCache / host KV offload, PD disaggregation, the MI355X AgentX preset, the PPU SGLang PD recipes, and the Vera Rubin and TPU recipes.

Only `mxfp4-vllm-nvidia-b300-tp8` has passed on platform hardware; the other profiles are taken from upstream and not yet verified here. The ModelScope repository is still empty, so weights come from Hugging Face only; a node without Hugging Face access needs the weights synced from another node or shared storage.
