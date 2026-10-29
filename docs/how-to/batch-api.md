# 运行批处理任务：Batch API 与控制台

本文说明怎样把一大批推理请求交给平台异步执行：准备 JSONL 文件、上传、创建批处理任务、查看进度、下载结果、取消，以及怎样把它指向离线服务等级的部署、只用空闲 GPU 跑。

**适用场景**：请求数量大、不要求即时返回的工作，例如给数据集批量生成回答、批量打标签、批量生成向量。需要逐字流式返回或毫秒级响应的请求不适合批处理，直接调用[OpenAI 兼容网关](../reference/openai-gateway.md)。

**前置条件**：

- 有一个运行中的部署，且你知道它对外的模型名（served model name）。批处理按每行请求里的 `model` 路由，和直接调用网关一样。
- 有推理令牌（`gw-` 开头）：在**API 令牌**页的**推理服务令牌**卡片创建，明文只显示一次。server 没有配置任何推理令牌时，`/v1` 路由与网关其他接口一样不要求鉴权。
- 在控制台里创建、取消批处理任务需要管理员（页面上对其他角色禁用创建按钮）；普通用户可以在控制台查看自己的任务。
- 接口规格（对象字段、错误码、限制）见 [REST API 全量路由](../reference/api.md#batches批处理任务)。

## 批处理任务做什么

一个批处理任务是一组来自同一个 JSONL 文件的请求。流程固定为三步：

1. 上传 JSONL 文件（`POST /v1/files`），平台边收边校验，通过后得到文件 ID。
2. 用这个文件创建批处理任务（`POST /v1/batches`），得到批处理任务 ID。
3. 任务结束后，从输出文件和错误文件取结果。

支持三个接口：`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings`；一个任务里的所有请求必须用同一个接口。不支持流式（`"stream": true`）。完成时限固定为 `24h`：任务创建后 24 小时内没有结果的请求，按过期处理。

任务的请求走与客户端请求相同的网关路径：按 `model` 选副本、必要时唤醒休眠的部署、计入用量和请求记录，所以它们会出现在请求视图里，也会重置部署的闲置休眠计时。

## 1. 准备输入文件

输入是 JSON Lines，文件名必须以 `.jsonl` 结尾（其他后缀上传时返回 400 `invalid_file_extension`）。每个非空行是一个请求，字段如下。

| 字段 | 要求 |
|---|---|
| `custom_id` | 非空字符串，在文件内唯一。结果按它对应回请求 |
| `method` | 固定为 `POST` |
| `url` | `/v1/chat/completions`、`/v1/completions`、`/v1/embeddings` 之一，创建任务时的 `endpoint` 必须与每一行相同 |
| `body` | 对象，其中 `model` 是非空字符串，且不能是 `"stream": true` |

示例 `requests.jsonl`（实际文件里每个请求占一行）：

```jsonl
{"custom_id":"q-001","method":"POST","url":"/v1/chat/completions","body":{"model":"qwen3-offline","messages":[{"role":"user","content":"Explain what a KV cache is in one sentence."}],"max_tokens":128}}
{"custom_id":"q-002","method":"POST","url":"/v1/chat/completions","body":{"model":"qwen3-offline","messages":[{"role":"user","content":"Translate hello into French."}],"max_tokens":32}}
```

文件上限：200 MB、50000 个请求（非空行），单行最长 16 MiB。上传时逐行校验，遇到第一处错误就拒绝，错误信息带行号，例如 `Line 2: custom_id must be a non-empty string`；被拒绝的上传不会留下任何文件。

## 2. 用 curl 跑一轮

以下命令使用 `GPUWORKS_URL` 和 `GPUWORKS_API_KEY` 两个环境变量，写法同[第一个模型的教程](../tutorials/getting-started.md#6-调用并验证回答)。

```bash
GPUWORKS_URL=http://192.0.2.10:8800
read -r -s -p '请粘贴推理令牌：' GPUWORKS_API_KEY
printf '\n'
```

### 上传文件

```bash
curl -sS "$GPUWORKS_URL/v1/files" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -F purpose=batch \
  -F file=@requests.jsonl
```

成功返回 200 和文件对象，记下 `id`：

```json
{"id":"file-3f9c2a7e1b6d4c8a90e5d721","object":"file","bytes":412,"created_at":1790000000,"filename":"requests.jsonl","purpose":"batch","status":"processed"}
```

`purpose` 只能是 `batch`。

### 创建批处理任务

```bash
curl -sS "$GPUWORKS_URL/v1/batches" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"purpose":"demo"}}'
```

返回批处理任务对象，初始状态是 `validating`：

```json
{"id":"batch_5a1e9d03c7b24f6e8d2a4b19","object":"batch","endpoint":"/v1/chat/completions","errors":null,"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","completion_window":"24h","status":"validating","output_file_id":null,"error_file_id":null,"created_at":1790000010,"in_progress_at":null,"expires_at":1790086410,"finalizing_at":null,"completed_at":null,"failed_at":null,"expired_at":null,"cancelling_at":null,"cancelled_at":null,"request_counts":{"total":2,"completed":0,"failed":0},"metadata":{"purpose":"demo"}}
```

`metadata` 可选：最多 16 个键，键不超过 64 个字符，值不超过 512 个字符，都是字符串。

### 轮询进度

```bash
BATCH_ID=batch_5a1e9d03c7b24f6e8d2a4b19
while true; do
  resp=$(curl -sS "$GPUWORKS_URL/v1/batches/$BATCH_ID" -H "Authorization: Bearer $GPUWORKS_API_KEY")
  echo "$resp" | jq -c '{status, request_counts}'
  case $(echo "$resp" | jq -r .status) in
    completed|failed|expired|cancelled) break ;;
  esac
  sleep 10
done
```

`request_counts` 约每 5 秒落库一次，所以计数是跳变的，不是逐请求递增；仍在重试的请求既不算 `completed` 也不算 `failed`。

### 下载结果

任务结束后，`output_file_id` 指向成功请求的输出文件，`error_file_id` 指向失败请求的错误文件；对应请求不存在时为 `null`。

```bash
OUT=$(echo "$resp" | jq -r .output_file_id)
curl -sS "$GPUWORKS_URL/v1/files/$OUT/content" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" -o output.jsonl
```

文件格式见[结果文件怎么读](#结果文件怎么读)。

## 3. 用 OpenAI Python SDK

官方 SDK 只需要把 `base_url` 指到 GPUWORKS 的 `/v1`：

```python
import time
from openai import OpenAI

client = OpenAI(base_url="http://192.0.2.10:8800/v1", api_key="gw-...")

batch_file = client.files.create(file=open("requests.jsonl", "rb"), purpose="batch")

batch = client.batches.create(
    input_file_id=batch_file.id,
    endpoint="/v1/chat/completions",
    completion_window="24h",
    metadata={"purpose": "demo"},
)

while batch.status not in ("completed", "failed", "expired", "cancelled"):
    time.sleep(10)
    batch = client.batches.retrieve(batch.id)
    print(batch.status, batch.request_counts)

if batch.output_file_id:
    client.files.content(batch.output_file_id).write_to_file("output.jsonl")
if batch.error_file_id:
    client.files.content(batch.error_file_id).write_to_file("errors.jsonl")
```

取消用 `client.batches.cancel(batch.id)`，列出用 `client.batches.list(limit=20)`。

## 4. 在控制台运行

左侧菜单**推理服务**组里的**批处理任务**页，不写代码也能跑一轮。页面顶部的**通过 API 调用**可展开，按当前地址给出上面 curl 与 SDK 的调用示例；**API 令牌**页的调用方式里也提示了这组接口。

1. 点**创建批处理任务**，选择 `.jsonl` 文件和端点（`endpoint`），提交。文件在上传时同样逐行校验，错误会指出行号和原因。没有现成文件时，在对话框的**示例文件**里选一个运行中的部署，点**下载示例**得到 5 个请求的 `.jsonl` 示例，或点**使用示例**直接用它创建任务。
2. 列表显示每个任务的 ID、状态、端点、进度（已完成/总数，含失败数）、创建时间和过期时间，可用**按状态筛选**缩小范围。
3. 点一行打开详情抽屉：概要、请求计数、各阶段时间线、元数据，以及**下载输入文件**、**下载输出文件**、**下载错误文件**。失败的任务（`failed`）还显示失败原因。
4. 任务未结束时点**取消任务**，在页内确认后生效。

在控制台创建的任务属于当前登录用户，和用 API 令牌创建的任务一样，可以用同一用户的任意推理令牌通过 `/v1/batches` 查看。管理员能看到所有用户的任务，普通用户只看到自己的。

## 状态、取消与过期

| 状态 | 含义 |
|---|---|
| `validating` | 已创建，排队等执行名额；全 server 同时最多执行 32 个任务 |
| `in_progress` | 正在发送请求 |
| `finalizing` | 请求都有了结果，正在写输出文件和错误文件 |
| `completed` | 结束，所有请求都有了结果（成功或失败） |
| `failed` | 平台故障，例如输入文件消失；`errors` 说明原因 |
| `expired` | 创建后过了 24 小时；还没有结果的请求写入错误文件，错误码 `batch_expired` |
| `cancelling` | 已请求取消：不再发送新请求，已在途的请求最多再等 2 分钟 |
| `cancelled` | 取消完成，已有的结果在输出文件和错误文件里 |

取消用 `POST /v1/batches/{id}/cancel`（控制台里是详情抽屉的**取消**）。对已经结束的任务取消返回 409 `batch_not_cancellable`。

任务的 `expires_at` 是创建时间加 24 小时。server 重启不会丢任务：未结束的任务从最近一次进度检查点继续，已经有结果的请求不会重发。

## 结果文件怎么读

输出文件和错误文件都是 JSON Lines，每个 `custom_id` 一行，按完成顺序排列，不按输入顺序，要对应请求时用 `custom_id`。

输出文件（`purpose` 为 `batch_output`）每行是一个成功的请求：

```json
{"id":"batch_req_8c3d51f0a2e7","custom_id":"q-001","response":{"status_code":200,"request_id":"req-7b2f","body":{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The KV cache stores the keys and values of processed tokens so they are not recomputed."},"finish_reason":"stop"}]}},"error":null}
```

错误文件每行是一个最终失败的请求：

```json
{"id":"batch_req_1d90be44c6a3","custom_id":"q-002","response":{"status_code":400,"request_id":"req-91ac","body":{"error":{"message":"max_tokens is too large","type":"BadRequestError","code":400}}},"error":{"code":"400","message":"max_tokens is too large"}}
```

- 模型服务返回 4xx 时，`response` 保留完整应答，`error.code` 和 `error.message` 取自模型服务的错误对象；错误对象没有 `code` 时，`error.code` 依次取它的 `type`、`http_<状态码>`。
- 其他错误码：`batch_expired`（到期前没有结果）和 `response_too_large`（应答超过网关为批处理保留的大小）。
- 输出文件只在至少有一个请求成功时存在，错误文件只在至少有一个请求失败时存在。
- 输出文件和错误文件在任务结束 30 天后删除（任务本身保留计数），之后读取返回 404。输入文件一直保留，直到你删除。

删除输入文件用 `DELETE /v1/files/{id}`；仍有未结束的任务在读它时返回 409 `file_in_use`。

## 哪些失败会自动重试

平台把“请求本身不对”和“平台暂时服务不了”分开：

| 情况 | 处理 |
|---|---|
| 应答是 HTTP 200，但某个 choice 的 `finish_reason` 为 `abort`（离线部署被更高等级抢卡） | 自动重试，不算失败 |
| 503（部署在休眠、挂起时间内没醒来，或还没有副本）、429、408、其他 5xx、连接错误 | 自动重试，不算失败 |
| 其他 4xx：400 请求无效、404 模型不存在、401 等 | 最终失败，写入错误文件，计入 `failed` |

重试用指数退避，从 2 秒翻倍到 60 秒封顶，遵守应答里的 `Retry-After` 并加随机抖动，一直重试到任务过期。模型名写错的任务，每一行都会很快得到 404 而进入错误文件，不会拖到过期。

## 与离线服务等级搭配

把批处理指向一个**离线**等级的部署，就能只用空闲 GPU 跑批：实时模型占卡时批处理部署让出显存，卡空闲了再自动恢复。部署侧怎么设等级见[服务等级与按需唤醒](scheduling-center.md#服务等级与按需唤醒)，规则见[服务等级](../explanation/service-levels.md)。

1. 创建部署时打开**可休眠**，把**服务等级**设为**离线**（或之后在部署详情的**调度策略**里修改）。
2. 在 JSONL 的 `model` 里写这个部署的模型名，其余步骤同上。

批处理与离线部署的交互：

- **按需唤醒**：部署此刻在休眠或被让卡时，批处理请求和普通请求一样被网关挂起并触发唤醒，醒后放行。离线等级最多挂起 30 分钟；超时的请求得到 503，批处理自动重试。
- **算用户活动**：批处理请求会重置闲置休眠计时，并出现在请求记录里，这一点不同于健康探测。部署因此不会在批处理中途被闲置休眠。
- **被抢占后重试**：更高等级的部署需要这些卡时，离线部署立即让卡，在途请求被截断，客户端看到 HTTP 200 加 `finish_reason: "abort"`。批处理识别这个信号，把这些请求重新发送，直到任务过期为止，它们不计为失败。卡空出来后部署自动恢复，批处理继续。
- **一直被占卡时的结果**：如果实时模型一直忙，离线部署一直拿不到卡，请求会持续重试直到任务过期，剩下的请求以 `batch_expired` 进入错误文件。需要更确定的完成时间时，给批处理换到常驻或分时部署，或缩小批量。

## 范围与限制

- 文件和任务属于创建它们的 API 令牌所属的用户；同一用户的任意推理令牌都能看到，其他用户得到 404。管理员在控制台能看到所有用户的任务。
- 同时执行：每个任务 8 个并发请求，所有任务合计 64 个在途请求。
- 文件存放在 server 数据目录的 `batch-files/` 下，占用 server 磁盘；`GET /v1/files` 可以列出自己的文件，不再需要的输入文件请删除。
- 完成时限只能是 `24h`。

## 排查

| 现象 | 检查 |
|---|---|
| 上传 400，带 `Line N` | 按行号和错误码修正文件，例如 `duplicate_custom_id`、`streaming_not_supported`、`invalid_url` |
| 创建任务 400 `endpoint_mismatch` | `endpoint` 与文件里每一行的 `url` 必须一致 |
| 创建任务 400 `file_not_found` | 文件 ID 写错，或不是同一用户的 `purpose=batch` 文件 |
| 任务一直 `validating` | 全 server 已有 32 个任务在执行，排队等名额 |
| 任务停在 `in_progress`、计数不动 | 部署可能在休眠或被让卡；看部署时间线和调度中心。请求在重试，不算失败 |
| 全部请求进了错误文件，状态码 404 | `model` 与部署对外的模型名不一致 |
| 读输出文件 404 | 任务已结束超过 30 天，文件已删除；或者没有成功的请求，输出文件本就不存在 |
| 401 `invalid_api_key` | 检查 `Authorization: Bearer <key>` |
