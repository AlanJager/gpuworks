"""Bounded offline harness for the Voxtral realtime end-of-audio fix.

The harness executes the verbatim upstream vLLM sources kept as digest-pinned
test fixtures (voxtral_realtime.py and the realtime websocket connection.py)
with stubbed vllm/mistral_common/numpy dependencies, and drives them through
the reported defect scenarios:

  - audio stream ending without trailing silence must flush the partial final
    frame instead of dropping the tail words;
  - a final-only input_audio_buffer.commit must start generation so the client
    receives transcription.delta events followed by transcription.done;
  - connection teardown must cancel AND await the generation task, and the
    model-side stream adapter must cancel AND gather its feeder tasks.

Every line of output is either "OK <scenario>" or
"DEFECT <scenario> <defect-id>: <detail>"; the process exits non-zero if any
scenario reports a defect. The Go test asserts which scenarios must report a
defect on the pristine fixtures (the bug being fixed) and that all scenarios
pass on the patched sources.
"""

import argparse
import ast
import asyncio
import base64 as stdlib_base64
import importlib.util
import json
import struct
import sys
import textwrap
import types
from collections.abc import AsyncGenerator, Iterable, Iterator
from http import HTTPStatus
from uuid import uuid4

SCENARIO_TIMEOUT_S = 10.0

# Bounded audio configuration mirroring the upstream formulas: 80 ms streaming
# steps of 1280 samples at 16 kHz, 16 ms (256 samples) of look-ahead/look-back
# overlap per frame and 320 samples per audio token.
SAMPLING_RATE = 16000
FRAME_RATE = 12.5
LOOK_AHEAD_MS = 16.0
LOOK_BACK_MS = 16.0
RAW_AUDIO_PER_TOKEN = 320
PROMPT_TOKENS = [101, 102]

FRAME0_SIZE = 896   # 640 initial + 256 look-ahead
FRAME_SIZE = 1792   # 256 look-back + 1280 step + 256 look-ahead
STRIDE0 = 384       # FRAME0_SIZE - look-ahead - look-back

LEFT_PAD_SAMPLES = 128
RIGHT_PAD_SAMPLES = 640

AUDIO_CONFIG = types.SimpleNamespace(
    sampling_rate=SAMPLING_RATE,
    frame_rate=FRAME_RATE,
    streaming_look_ahead_ms=LOOK_AHEAD_MS,
    streaming_look_back_ms=LOOK_BACK_MS,
    raw_audio_length_per_tok=RAW_AUDIO_PER_TOKEN,
)


class Defect(Exception):
    def __init__(self, defect_id, detail):
        super().__init__(detail)
        self.defect_id = defect_id


# --- numpy stand-in -------------------------------------------------------
# Only the 1-D operations the fixtures use are mirrored: len, slicing,
# concatenate, zeros, frombuffer(int16).astype(float32)/32768.0 and .dtype.


class _NDArray:
    __slots__ = ("values", "dtype")

    def __init__(self, values, dtype):
        self.values = list(values)
        self.dtype = dtype

    def __len__(self):
        return len(self.values)

    def __getitem__(self, key):
        if isinstance(key, slice):
            return _NDArray(self.values[key], self.dtype)
        return self.values[key]

    def astype(self, dtype):
        return _NDArray([float(v) for v in self.values], dtype)

    def __truediv__(self, scalar):
        return _NDArray([v / scalar for v in self.values], self.dtype)


class _NPShim:
    ndarray = _NDArray
    int16 = "int16"
    float32 = "float32"

    @staticmethod
    def concatenate(arrays):
        values = []
        for array in arrays:
            values.extend(array.values)
        return _NDArray(values, arrays[0].dtype)

    @staticmethod
    def zeros(count, dtype):
        return _NDArray([0.0] * int(count), dtype)

    @staticmethod
    def frombuffer(buffer, dtype):
        count = len(buffer) // 2
        return _NDArray(struct.unpack("<%dh" % count, buffer[: count * 2]), dtype)


def samples(values):
    return _NDArray(values, "float32")


def ramp(count, start=0.0, step=0.001):
    return _NDArray([start + step * i for i in range(count)], "float32")


# --- asyncio gather spy ----------------------------------------------------
# The lifecycle defect is a fire-and-forget cancel(): teardown must also await
# the cancelled tasks. Recording gather calls observes exactly that contract.


class _AsyncioSpy:
    def __init__(self):
        self.gather_calls = []

    def __getattr__(self, name):
        return getattr(asyncio, name)

    def gather(self, *args, **kwargs):
        self.gather_calls.append(args)
        return asyncio.gather(*args, **kwargs)


# --- source loading --------------------------------------------------------


def load_class(path, class_name, namespace):
    text = open(path, encoding="utf-8").read()
    tree = ast.parse(text)
    node = next(
        n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == class_name
    )
    segment = ast.get_source_segment(text, node)
    exec(compile(segment, path, "exec"), namespace)
    return namespace[class_name]


def load_method(path, class_name, method_name, namespace):
    text = open(path, encoding="utf-8").read()
    tree = ast.parse(text)
    cls = next(
        n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == class_name
    )
    fn = next(
        n
        for n in cls.body
        if isinstance(n, ast.AsyncFunctionDef) and n.name == method_name
    )
    segment = textwrap.dedent(ast.get_source_segment(text, fn))
    lines = [line for line in segment.splitlines() if not line.strip().startswith("@")]
    exec(compile("\n".join(lines), path, "exec"), namespace)
    return namespace[method_name]


# --- shared stub types ------------------------------------------------------


class _TokensPrompt:
    def __init__(self, prompt_token_ids=None, multi_modal_data=None):
        self.prompt_token_ids = prompt_token_ids
        self.multi_modal_data = multi_modal_data


class _StreamingInput:
    def __init__(self, prompt):
        self.prompt = prompt


# --- VoxtralRealtimeBuffer scenarios ----------------------------------------


def load_buffer_class(path):
    np = _NPShim()
    namespace = {
        "asyncio": asyncio,
        "np": np,
        "Iterator": Iterator,
        "AsyncGenerator": AsyncGenerator,
        "Iterable": Iterable,
        "AudioConfig": object,
        "StreamingInput": _StreamingInput,
        "TokensPrompt": _TokensPrompt,
    }
    return load_class(path, "VoxtralRealtimeBuffer", namespace), np


async def collect_frames(path, chunks, cancel_after_frames=None):
    """Feed chunks (no implicit EOF when cancel_after_frames is set) and return
    the audio frame of every StreamingInput the buffer yields."""
    cls, np = load_buffer_class(path)
    buffer = cls(AUDIO_CONFIG, list(PROMPT_TOKENS))
    for chunk in chunks:
        await buffer.append_audio(chunk)
    stream = buffer.get_input_stream()
    frames = []
    if cancel_after_frames is None:
        await buffer.append_audio(None)
        async for item in stream:
            frames.append(item.prompt.multi_modal_data["audio"][0])
            # Mirror the engine's token feedback: one streaming step's worth of
            # tokens per emitted frame, like feed_tokens does from the output.
            for token in range(4):
                await buffer.append_tokens([2000 + token])
        return frames
    iterator = stream.__aiter__()
    for _ in range(cancel_after_frames):
        frames.append((await iterator.__anext__()).prompt.multi_modal_data["audio"][0])
    pending = asyncio.ensure_future(iterator.__anext__())
    await asyncio.sleep(0.05)
    pending.cancel()
    try:
        await pending
        raise Defect("cancel-ignored", "buffer kept waiting after cancellation")
    except asyncio.CancelledError:
        pass
    await stream.aclose()
    return frames


async def scenario_buffer_empty(path):
    frames = await collect_frames(path, [])
    if frames:
        raise Defect("empty-buffer-emitted-frame", f"empty stream emitted {len(frames)} frames")


async def scenario_buffer_partial_frame_tail(path):
    audio = ramp(1200)
    frames = await collect_frames(path, [audio])
    if len(frames) != 2:
        raise Defect(
            "tail-frame-dropped",
            f"1200-sample stream without trailing silence produced {len(frames)} frames; "
            "the 304 samples past the first frame's overlap were never decoded",
        )
    final = frames[1]
    if len(final) != FRAME_SIZE:
        raise Defect("tail-frame-dropped", f"final frame has {len(final)} samples, want {FRAME_SIZE}")
    tail = 1200 - STRIDE0
    if final.values[:tail] != audio.values[STRIDE0:]:
        raise Defect("tail-frame-dropped", "final frame does not contain the undecoded tail audio")
    if any(v != 0.0 for v in final.values[tail:]):
        raise Defect("tail-frame-dropped", "final frame is not zero-padded with silence at EOF")


async def scenario_buffer_step_aligned_tail(path):
    audio = ramp(1920)
    frames = await collect_frames(path, [audio])
    if len(frames) != 2:
        raise Defect(
            "tail-frame-dropped",
            f"1920-sample stream produced {len(frames)} frames; samples [640,1920) were never decoded",
        )
    tail = 1920 - STRIDE0
    if frames[1].values[:tail] != audio.values[STRIDE0:]:
        raise Defect("tail-frame-dropped", "final frame does not contain the tail audio")


async def scenario_buffer_overlap_only_tail(path):
    audio = ramp(FRAME0_SIZE)
    frames = await collect_frames(path, [audio])
    if len(frames) != 1:
        raise Defect(
            "overlap-tail-duplicated",
            f"stream ending exactly at a frame boundary produced {len(frames)} frames; "
            "the overlap region must not be decoded twice",
        )
    if frames[0].values != audio.values:
        raise Defect("overlap-tail-duplicated", "single frame content mismatch")


async def scenario_buffer_multi_chunk_tail(path):
    audio = ramp(1200)
    chunks = [
        _NDArray(audio.values[:500], "float32"),
        _NDArray(audio.values[500:900], "float32"),
        _NDArray(audio.values[900:], "float32"),
    ]
    frames = await collect_frames(path, chunks)
    if len(frames) != 2:
        raise Defect(
            "tail-frame-dropped",
            f"3-chunk 1200-sample stream produced {len(frames)} frames",
        )
    tail = 1200 - STRIDE0
    if frames[1].values[:tail] != audio.values[STRIDE0:]:
        raise Defect("tail-frame-dropped", "final frame lost tail audio across chunk boundaries")


async def scenario_buffer_cancel_mid_frame(path):
    frames = await collect_frames(path, [ramp(1200)], cancel_after_frames=1)
    if len(frames) != 1:
        raise Defect("cancel-ignored", f"expected exactly one frame before cancel, got {len(frames)}")


# --- RealtimeConnection scenarios -------------------------------------------


class _WebSocketDisconnect(Exception):
    pass


class _Event:
    event_type = ""

    def model_dump_json(self):
        return json.dumps({"type": self.event_type, **self.__dict__})


class _SessionCreated(_Event):
    event_type = "session.created"


class _TranscriptionDelta(_Event):
    event_type = "transcription.delta"

    def __init__(self, delta=""):
        self.delta = delta


class _TranscriptionDone(_Event):
    event_type = "transcription.done"

    def __init__(self, text="", usage=None):
        self.text = text
        self.usage = usage

    def model_dump_json(self):
        return json.dumps({
            "type": self.event_type,
            "text": self.text,
            "usage": vars(self.usage) if self.usage is not None else None,
        })


class _ErrorEvent(_Event):
    event_type = "error"

    def __init__(self, error="", code=None):
        self.error = error
        self.code = code


class _InputAudioBufferAppend:
    def __init__(self, type=None, audio=None, **rest):
        self.audio = audio


class _InputAudioBufferCommit:
    def __init__(self, type=None, final=False, **rest):
        self.final = final


class _UsageInfo:
    def __init__(self, prompt_tokens=0, completion_tokens=0, total_tokens=0):
        self.prompt_tokens = prompt_tokens
        self.completion_tokens = completion_tokens
        self.total_tokens = total_tokens


class _ValidationError(Exception):
    def __init__(self, message, parameter=None, value=None):
        super().__init__(message)


class _ErrorResponse:
    def __init__(self, message):
        self.error = types.SimpleNamespace(message=message)


_LOGGER = types.SimpleNamespace(
    debug=lambda *a, **k: None,
    info=lambda *a, **k: None,
    warning=lambda *a, **k: None,
    error=lambda *a, **k: None,
    exception=lambda *a, **k: None,
)


def install_fake_vllm_sampling_params():
    module = types.ModuleType("vllm")
    sampling = types.ModuleType("vllm.sampling_params")

    class _RequestOutputKind:
        DELTA = "delta"

    class _SamplingParams:
        @classmethod
        def from_optional(cls, **kwargs):
            return types.SimpleNamespace(**kwargs)

    sampling.RequestOutputKind = _RequestOutputKind
    sampling.SamplingParams = _SamplingParams
    sys.modules.setdefault("vllm", module)
    sys.modules["vllm.sampling_params"] = sampling


class _FakeOutput:
    def __init__(self, text, token_ids, prompt_token_ids):
        self.outputs = [types.SimpleNamespace(text=text, token_ids=token_ids)]
        self.prompt_token_ids = prompt_token_ids


class _FakeEngineClient:
    async def generate(self, prompt, sampling_params, request_id):
        index = 0
        async for _ in prompt:
            yield _FakeOutput(f"w{index} ", [1000 + index], [1, 2, 3])
            index += 1


class _FakeServing:
    def __init__(self):
        self.engine_client = _FakeEngineClient()
        self.model_cls = types.SimpleNamespace(realtime_max_tokens=16)

    def _is_model_supported(self, model):
        return True

    def create_error_response(self, message, err_type, status_code, param):
        return _ErrorResponse(message)

    def transcribe_realtime(self, audio_stream, input_stream):
        # Matches the real serving method: a plain function returning an async
        # generator, which start_generation passes straight to engine.generate.
        async def gen():
            async for chunk in audio_stream:
                yield chunk

        return gen()


class _FakeWebSocket:
    def __init__(self, inbound):
        self.inbound = list(inbound)
        self.sent = []
        self.accepted = False
        self._release = asyncio.Event()

    async def accept(self):
        self.accepted = True

    async def receive_text(self):
        if self.inbound:
            return self.inbound.pop(0)
        await self._release.wait()
        raise _WebSocketDisconnect()

    async def send_text(self, data):
        self.sent.append(json.loads(data))

    def disconnect(self):
        self._release.set()


def append_event(sample_count=640):
    pcm = struct.pack("<%dh" % sample_count, *([100] * sample_count))
    return json.dumps(
        {"type": "input_audio_buffer.append", "audio": stdlib_base64.b64encode(pcm).decode()}
    )


SESSION_UPDATE = json.dumps({"type": "session.update", "model": "voxtral-realtime"})
COMMIT = json.dumps({"type": "input_audio_buffer.commit"})
COMMIT_FINAL = json.dumps({"type": "input_audio_buffer.commit", "final": True})


def load_connection_class(spy):
    np = _NPShim()
    namespace = {
        "asyncio": spy,
        "json": json,
        "np": np,
        "base64": stdlib_base64,
        "envs": types.SimpleNamespace(VLLM_MAX_AUDIO_CLIP_FILESIZE_MB=25),
        "ErrorResponse": _ErrorResponse,
        "UsageInfo": _UsageInfo,
        "VLLMValidationError": _ValidationError,
        "logger": _LOGGER,
        "ErrorEvent": _ErrorEvent,
        "InputAudioBufferAppend": _InputAudioBufferAppend,
        "InputAudioBufferCommit": _InputAudioBufferCommit,
        "SessionCreated": _SessionCreated,
        "TranscriptionDelta": _TranscriptionDelta,
        "TranscriptionDone": _TranscriptionDone,
        "WebSocketDisconnect": _WebSocketDisconnect,
        "WebSocket": object,
        "OpenAIServingRealtime": object,
        "HTTPStatus": HTTPStatus,
        "uuid4": uuid4,
        "AsyncGenerator": AsyncGenerator,
    }
    return namespace


async def run_connection(path, inbound, wait_for):
    install_fake_vllm_sampling_params()
    spy = _AsyncioSpy()
    namespace = load_connection_class(spy)
    cls = load_class(path, "RealtimeConnection", namespace)
    websocket = _FakeWebSocket(inbound)
    connection = cls(websocket, _FakeServing())
    task = asyncio.ensure_future(connection.handle_connection())
    deadline = asyncio.get_event_loop().time() + 1.0
    while not wait_for(websocket) and asyncio.get_event_loop().time() < deadline:
        await asyncio.sleep(0.01)
    websocket.disconnect()
    await asyncio.wait_for(task, SCENARIO_TIMEOUT_S)
    return connection, websocket, spy


def event_types(websocket):
    return [event.get("type") for event in websocket.sent]


async def scenario_connection_final_commit_only(path):
    def done_seen(ws):
        return "transcription.done" in event_types(ws)

    connection, websocket, spy = await run_connection(
        path, [SESSION_UPDATE, append_event(), COMMIT_FINAL], done_seen
    )
    types_seen = event_types(websocket)
    if "transcription.done" not in types_seen:
        raise Defect(
            "missing-completion-event",
            f"final-only commit produced no transcription events: {types_seen}",
        )
    if types_seen[0] != "session.created":
        raise Defect("missing-completion-event", f"first event is {types_seen[0]}")
    done_index = types_seen.index("transcription.done")
    deltas = types_seen[1:done_index]
    if not deltas or any(t != "transcription.delta" for t in deltas):
        raise Defect("missing-completion-event", f"no delta events before done: {types_seen}")
    if any(t == "transcription.delta" for t in types_seen[done_index + 1:]):
        raise Defect("missing-completion-event", "delta event after transcription.done")
    done = websocket.sent[done_index]
    expected_text = "".join(e["delta"] for e in websocket.sent[1:done_index])
    if done["text"] != expected_text or not expected_text:
        raise Defect("missing-completion-event", f"done text {done['text']!r} != deltas {expected_text!r}")
    if connection.generation_task is None or not connection.generation_task.done():
        raise Defect("cleanup-does-not-await-generation", "generation task still pending after teardown")


async def scenario_connection_empty_final_commit(path):
    def done_seen(ws):
        return "transcription.done" in event_types(ws)

    connection, websocket, spy = await run_connection(path, [SESSION_UPDATE, COMMIT_FINAL], done_seen)
    types_seen = event_types(websocket)
    if types_seen != ["session.created", "transcription.done"]:
        raise Defect(
            "missing-completion-event",
            f"empty final commit produced {types_seen}; want session.created + transcription.done",
        )
    if websocket.sent[1]["text"] != "":
        raise Defect("missing-completion-event", "empty audio must produce empty done text")


async def scenario_connection_commit_before_session(path):
    connection, websocket, spy = await run_connection(path, [COMMIT_FINAL], lambda ws: len(ws.sent) >= 2)
    types_seen = event_types(websocket)
    if types_seen != ["session.created", "error"]:
        raise Defect("wrong-validation", f"commit before session.update produced {types_seen}")
    if websocket.sent[1].get("code") != "model_not_validated":
        raise Defect("wrong-validation", f"unexpected error event {websocket.sent[1]}")
    if connection.generation_task is not None:
        raise Defect("wrong-validation", "generation started without a validated session")


async def scenario_connection_peer_disconnect_mid_generation(path):
    spy_holder = {}

    def two_deltas(ws):
        return event_types(ws).count("transcription.delta") >= 1

    connection, websocket, spy = await run_connection(
        path, [SESSION_UPDATE, append_event(), COMMIT], two_deltas
    )
    # The peer disconnected while generation was waiting for more audio: the
    # teardown must have cancelled AND awaited the generation task.
    if connection.generation_task is None:
        raise Defect("cleanup-does-not-await-generation", "generation never started after commit")
    if not any(connection.generation_task in call for call in spy.gather_calls):
        raise Defect(
            "cleanup-does-not-await-generation",
            "cleanup cancelled the generation task without awaiting it",
        )
    if not connection.generation_task.done():
        raise Defect("cleanup-does-not-await-generation", "generation task survived connection teardown")


async def scenario_connection_cancel_connection_mid_generation(path):
    install_fake_vllm_sampling_params()
    spy = _AsyncioSpy()
    namespace = load_connection_class(spy)
    cls = load_class(path, "RealtimeConnection", namespace)
    websocket = _FakeWebSocket([SESSION_UPDATE, append_event(), COMMIT])
    connection = cls(websocket, _FakeServing())
    task = asyncio.ensure_future(connection.handle_connection())
    deadline = asyncio.get_event_loop().time() + 1.0
    while "transcription.delta" not in event_types(websocket):
        if asyncio.get_event_loop().time() > deadline:
            raise Defect("missing-completion-event", "generation never started for non-final commit")
        await asyncio.sleep(0.01)
    task.cancel()
    try:
        await task
        raise Defect("cancel-ignored", "handle_connection ignored cancellation")
    except asyncio.CancelledError:
        pass
    if connection.generation_task is None:
        raise Defect("cleanup-does-not-await-generation", "generation never started")
    if not any(connection.generation_task in call for call in spy.gather_calls):
        raise Defect(
            "cleanup-does-not-await-generation",
            "cancelled connection left the generation task detached",
        )
    if not connection.generation_task.done():
        raise Defect("cleanup-does-not-await-generation", "generation task still pending")


# --- buffer_realtime_audio stream scenarios ---------------------------------


def stream_namespace(spy, buffer_cls):
    left_pad = types.SimpleNamespace(audio_array=_NDArray([0.0] * LEFT_PAD_SAMPLES, "float32"))
    right_pad = types.SimpleNamespace(audio_array=_NDArray([0.0] * RIGHT_PAD_SAMPLES, "float32"))
    audio_encoder = types.SimpleNamespace(
        audio_config=AUDIO_CONFIG,
        encode_streaming_tokens=lambda: [7],
        get_padding_audio=lambda: (left_pad, right_pad),
    )
    tokenizer = types.SimpleNamespace(
        instruct=types.SimpleNamespace(
            start=lambda: list(PROMPT_TOKENS),
            audio_encoder=audio_encoder,
        ),
    )
    return {
        "asyncio": spy,
        "np": _NPShim(),
        "cached_tokenizer_from_config": lambda model_config: tokenizer,
        "VoxtralRealtimeBuffer": buffer_cls,
        "VLLM_ENGINE_ITERATION_TIMEOUT_S": 1,
        "AsyncGenerator": AsyncGenerator,
        "Iterable": Iterable,
        "PromptType": object,
        "ModelConfig": object,
    }


async def finite_audio_stream(chunks):
    for chunk in chunks:
        yield chunk


async def hanging_audio_stream(chunks):
    for chunk in chunks:
        yield chunk
    await asyncio.Event().wait()


async def run_stream(path, chunks, hanging=False, cancel=False):
    spy = _AsyncioSpy()
    buffer_cls, np = load_buffer_class(path)
    namespace = stream_namespace(spy, buffer_cls)
    function = load_method(path, "VoxtralRealtimeGeneration", "buffer_realtime_audio", namespace)
    input_stream = asyncio.Queue()
    stream = hanging_audio_stream(chunks) if hanging else finite_audio_stream(chunks)
    prompts = []

    async def consume():
        async for prompt in function(None, stream, input_stream, None):
            prompts.append(prompt)
            for _ in range(4):
                input_stream.put_nowait([999])
        return len(prompts)

    task = asyncio.ensure_future(consume())
    if cancel:
        await asyncio.sleep(0.1)
        task.cancel()
        try:
            await task
            raise Defect("cancel-ignored", "stream consumer ignored cancellation")
        except asyncio.CancelledError:
            pass
    else:
        await asyncio.wait_for(task, SCENARIO_TIMEOUT_S)
    return prompts, spy


async def scenario_stream_flush_tail(path):
    prompts, spy = await run_stream(path, [ramp(1200)])
    if len(prompts) != 2:
        raise Defect(
            "tail-frame-dropped",
            f"model-side stream yielded {len(prompts)} prompts; the final partial frame was dropped",
        )
    if not spy.gather_calls:
        raise Defect("cleanup-does-not-await-tasks", "stream teardown never gathered its feeder tasks")


async def scenario_stream_empty_commit(path):
    prompts, spy = await run_stream(path, [])
    if len(prompts) != 1:
        raise Defect(
            "missing-final-frame",
            f"empty stream yielded {len(prompts)} prompts; the right-padding frame must still be emitted",
        )
    if not spy.gather_calls:
        raise Defect("cleanup-does-not-await-tasks", "stream teardown never gathered its feeder tasks")


async def scenario_stream_cancel_mid_frame(path):
    prompts, spy = await run_stream(path, [ramp(500)], hanging=True, cancel=True)
    if prompts:
        raise Defect("cancel-ignored", f"cancelled stream yielded {len(prompts)} prompts")
    if not spy.gather_calls:
        raise Defect(
            "cleanup-does-not-await-tasks",
            "cancelled stream left audio/token feeder tasks detached",
        )


# --- driver ------------------------------------------------------------------

BUFFER_SCENARIOS = {
    "buffer/empty-buffer": scenario_buffer_empty,
    "buffer/partial-frame-tail": scenario_buffer_partial_frame_tail,
    "buffer/step-aligned-tail": scenario_buffer_step_aligned_tail,
    "buffer/overlap-only-tail": scenario_buffer_overlap_only_tail,
    "buffer/multi-chunk-tail": scenario_buffer_multi_chunk_tail,
    "buffer/cancel-mid-frame": scenario_buffer_cancel_mid_frame,
}

CONNECTION_SCENARIOS = {
    "connection/final-commit-only": scenario_connection_final_commit_only,
    "connection/empty-final-commit": scenario_connection_empty_final_commit,
    "connection/commit-before-session": scenario_connection_commit_before_session,
    "connection/peer-disconnect-mid-generation": scenario_connection_peer_disconnect_mid_generation,
    "connection/cancel-connection-mid-generation": scenario_connection_cancel_connection_mid_generation,
}

STREAM_SCENARIOS = {
    "stream/stream-flush-tail": scenario_stream_flush_tail,
    "stream/stream-empty-commit": scenario_stream_empty_commit,
    "stream/stream-cancel-mid-frame": scenario_stream_cancel_mid_frame,
}


def run_scenarios(groups):
    defects = 0
    for scenarios in groups:
        for name, scenario in scenarios.items():
            try:
                asyncio.run(asyncio.wait_for(scenario(CURRENT_SOURCE), SCENARIO_TIMEOUT_S))
            except Defect as defect:
                defects += 1
                print(f"DEFECT {name} {defect.defect_id}: {defect}")
            except Exception as error:
                defects += 1
                print(f"DEFECT {name} harness-error: {type(error).__name__}: {error}")
            else:
                print(f"OK {name}")
    return defects


CURRENT_SOURCE = ""


def main():
    global CURRENT_SOURCE
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["buffer", "connection", "stream", "all", "patch"])
    parser.add_argument("path", help="fixture source file, or patch script for mode=patch")
    parser.add_argument("root", nargs="?", help="source tree to patch (mode=patch)")
    args = parser.parse_args()

    if args.mode == "patch":
        spec = importlib.util.spec_from_file_location("voxtral_realtime_patch", args.path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        try:
            module.patch_root(args.root)
        except Exception as error:
            print(f"DEFECT patch patcher-rejected: {type(error).__name__}: {error}")
            return 1
        print("OK patch")
        return 0

    CURRENT_SOURCE = args.path
    groups = []
    if args.mode in ("buffer", "all"):
        groups.append(BUFFER_SCENARIOS)
    if args.mode in ("connection", "all"):
        groups.append(CONNECTION_SCENARIOS)
    if args.mode in ("stream", "all"):
        groups.append(STREAM_SCENARIOS)
    return 1 if run_scenarios(groups) else 0


if __name__ == "__main__":
    sys.exit(main())
