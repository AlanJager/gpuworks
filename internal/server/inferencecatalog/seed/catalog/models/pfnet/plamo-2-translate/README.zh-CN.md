# PLaMo 2 Translate

Preferred Networks 的翻译专用模型（约 10B 参数，BF16 权重约 19 GB）。它**没有做对话微调**，也没有聊天模板，只能用**文本补全接口**（`/v1/completions`）按固定格式调用；用对话接口或随手写的一句话去问，它不会翻译。上游只测试过英语与日语互译（本平台实测中→英也可用，见下）。

## 运行镜像

vLLM 从 0.27 起移除了 PLaMo 2 的实现，0.30 上会退回 Transformers 后端并加载失败，所以 NVIDIA 方案用官方镜像 `vllm/vllm-openai:v0.26.0`（约 10 GB，在 Docker Hub 上）。国内节点直连 Docker Hub 通常拉不动，需要给节点的容器运行时配代理，或先把镜像转存到能访问的仓库，再在「拉取设置」里加一条镜像规则。

## 提示词格式

```
<|plamo:op|>dataset
translation
<|plamo:op|>input lang=English
要翻译的原文
<|plamo:op|>output lang=Japanese
```

- 原文写在 `input lang=…` 的下一行，可以多行；语言写英文名，如 `lang=English`、`lang=Japanese`。
- 提示词必须以 `<|plamo:op|>output lang=目标语言` 加一个换行结尾，模型从这里开始写译文。
- 译文写完后模型会输出下一个 `<|plamo:op|>`，所以请求要带 `"stop": ["<|plamo:op|>"]`，否则会接着生成无关内容。上游示例用 `temperature: 0`。

本模板已把 `stop` 与 `temperature: 0` 设为请求默认值，试验场「补全」页也会预填一段英译日的提示词，直接点「生成」就能看到结果，再按上面的格式改原文和语言。

## 实测（阿里云 L20 × 1，vLLM 0.26.0，2026-10-20）

经试验场的补全接口发送，带本模板的默认参数（`stop`、`temperature: 0`），`max_tokens: 256`：

| 输入 | 结果 |
| --- | --- |
| 试验场预填的标签提示词（英→日） | 「GPUクラスタを活用することで、大規模言語モデルの展開とサービス提供が容易になります。」，15 token 后停止 |
| 标签格式，日→英 | "It's a nice day today, so let's take a walk in the park."，14 token 后停止 |
| 标签格式，中→英（上游未测中文） | "It's nice weather today, so let's go to the park for a walk."，13 token 后停止 |
| 只输入一句英文 | 不翻译，把它当成文章开头一路续写，写满 256 token |
| 中文指令“把这句话翻译成英文：…” | 先复述原句，再以对话口吻给出译文、重复一遍并附加多余的说明 |
| 通用的补全提示词“The capital of France is” | 回答 Paris 后反复重复同一句，写满 256 token |

也就是说，不按格式写时模型不会报错，但给不出可用的译文；对话接口不在本模板声明的接口里，试验场直接拒绝。

## 调用示例

```bash
curl "$GPUWORKS_URL/v1/completions" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H "Content-Type: application/json" \
  --data '{
    "model": "<served-model>",
    "prompt": "<|plamo:op|>dataset\ntranslation\n<|plamo:op|>input lang=English\nGPU clusters make it easy to deploy and serve large language models.\n<|plamo:op|>output lang=Japanese\n",
    "max_tokens": 256,
    "temperature": 0,
    "stop": ["<|plamo:op|>"]
  }'
```

```python
import os, requests

def translate(text, source="English", target="Japanese"):
    prompt = (
        "<|plamo:op|>dataset\ntranslation\n"
        f"<|plamo:op|>input lang={source}\n{text}\n"
        f"<|plamo:op|>output lang={target}\n"
    )
    reply = requests.post(
        os.environ["GPUWORKS_URL"].rstrip("/") + "/v1/completions",
        headers={"Authorization": f"Bearer {os.environ['GPUWORKS_API_KEY']}"},
        json={"model": "<served-model>", "prompt": prompt, "max_tokens": 1024, "temperature": 0, "stop": ["<|plamo:op|>"]},
    )
    reply.raise_for_status()
    return reply.json()["choices"][0]["text"].strip()

print(translate("GPU clusters make it easy to deploy and serve large language models."))
```

## 许可证

PLaMo community license。商业使用前须通过 Preferred Networks 提供的表单联系对方，见[模型主页](https://huggingface.co/pfnet/plamo-2-translate)。
