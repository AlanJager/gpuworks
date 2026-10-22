# PLaMo 2 Translate

Preferred Networks' translation model (about 10B parameters, about 19 GB of BF16 weights). It is **not tuned for chat** and has no chat template: it is driven through the **text completion API** (`/v1/completions`) with a fixed prompt format. Asked through the chat API, or with a plain sentence, it does not translate. Upstream has tested English and Japanese only (Chinese to English also worked here, see below).

## Runtime image

vLLM removed its PLaMo 2 implementation in 0.27; on 0.30 the model falls back to the Transformers backend and fails to load. The NVIDIA recipe therefore uses the official `vllm/vllm-openai:v0.26.0` image (about 10 GB, on Docker Hub). Nodes in mainland China usually cannot pull from Docker Hub directly: give the node's container runtime a proxy, or copy the image to a registry they reach and add an image rule under Pull Settings.

## Prompt format

```
<|plamo:op|>dataset
translation
<|plamo:op|>input lang=English
The text to translate
<|plamo:op|>output lang=Japanese
```

- The source text goes on the line after `input lang=…` and may span lines; languages are written by English name, such as `lang=English` and `lang=Japanese`.
- The prompt must end with `<|plamo:op|>output lang=<target>` and a newline: the model writes the translation from there.
- After the translation the model emits the next `<|plamo:op|>`, so requests carry `"stop": ["<|plamo:op|>"]`; without it the model goes on with unrelated text. Upstream uses `temperature: 0`.

This template sets `stop` and `temperature: 0` as request defaults, and the Playground's Completions page starts with an English-to-Japanese prompt: press Generate to see a translation, then change the text and languages in the format above.

## Measured (Aliyun L20 × 1, vLLM 0.26.0, 2026-10-20)

Sent through the Playground's completion API with this template's request defaults (`stop`, `temperature: 0`) and `max_tokens: 256`:

| Input | Result |
| --- | --- |
| The Playground's prefilled tagged prompt (English → Japanese) | 「GPUクラスタを活用することで、大規模言語モデルの展開とサービス提供が容易になります。」, stopped after 15 tokens |
| Tagged, Japanese → English | "It's a nice day today, so let's take a walk in the park.", stopped after 14 tokens |
| Tagged, Chinese → English (not tested upstream) | "It's nice weather today, so let's go to the park for a walk.", stopped after 13 tokens |
| A plain English sentence | No translation: continued as the opening of an article for all 256 tokens |
| A Chinese instruction ("translate this into English: …") | Repeated the source, then answered chat-style with the translation, a duplicate and unrequested remarks |
| The generic completion prompt "The capital of France is" | Answered Paris, then repeated the sentence for all 256 tokens |

Without the format the model does not fail, but gives no usable translation. The chat API is not among this template's operations, and the Playground refuses it.

## Examples

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

## License

PLaMo community license. Commercial use requires contacting Preferred Networks through their form first; see the [model page](https://huggingface.co/pfnet/plamo-2-translate).
