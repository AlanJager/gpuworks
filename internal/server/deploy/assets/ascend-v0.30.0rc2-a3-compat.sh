set -eu
GPUWORKS_V1_RENDEZVOUS_GENERATION="$(python3 - <<'PY'
import secrets
print(secrets.token_hex(32))
PY
)"
export GPUWORKS_V1_RENDEZVOUS_GENERATION
python3 - <<'PY'
from pathlib import Path
import re

runner = Path("/vllm-workspace/vllm-ascend/vllm_ascend/worker/model_runner_v1.py")
source = runner.read_text(encoding="utf-8")
pattern = re.compile(
    r"(?m)^        with _torch_cuda_wrapper\(\):\n"
    r"            super\(\).__init__\(vllm_config, device\)$"
)
replacement = (
    "        print(f\"GPUWorks: NPUModelRunner initializing on {device}\", flush=True)\n"
    "        with _torch_cuda_wrapper():\n"
    "            with torch.npu.device(device):\n"
    "                super().__init__(vllm_config, device)\n"
    "        print(\"GPUWorks: NPUModelRunner parent initialized\", flush=True)"
)
source, changed = pattern.subn(replacement, source, count=1)
if changed:
    runner.write_text(source, encoding="utf-8")
    print("GPUWorks: set the NPU device context before NPUModelRunner init")
elif "with torch.npu.device(device):" not in source:
    raise SystemExit("GPUWorks: unsupported vLLM-Ascend NPUModelRunner layout")
PY
python3 - <<'PY'
from pathlib import Path

worker = Path("/vllm-workspace/vllm-ascend/vllm_ascend/worker/worker.py")
source = worker.read_text(encoding="utf-8")
handler_before = "import torch\n"
handler_after = '''import torch

def _gpuworks_process_rendezvous(rank: int, world_size: int, key: str, phase: str) -> None:
    import hashlib
    import hmac
    import os
    import socket
    import stat
    import tempfile
    import time

    if world_size < 1 or rank < 0 or rank >= world_size:
        raise ValueError("GPUWorks rendezvous rank/world size is invalid")
    generation = os.environ.get("GPUWORKS_V1_RENDEZVOUS_GENERATION", "")
    try:
        secret = bytes.fromhex(generation)
    except ValueError:
        secret = b""
    if len(secret) < 32:
        raise RuntimeError("GPUWorks rendezvous is missing its pre-start generation secret")

    # The pre-start secret separates every container start, even when key, phase
    # and TMPDIR all repeat. Only ranks sharing that secret derive this address.
    key_bytes = key.encode("utf-8")
    phase_bytes = phase.encode("utf-8")
    material = (
        len(secret).to_bytes(8, "big") + secret
        + len(key_bytes).to_bytes(8, "big") + key_bytes + phase_bytes
    )
    identity = hashlib.sha256(material).digest()
    token = hmac.new(secret, b"GPUWorks rendezvous token" + identity, hashlib.sha256).digest()
    release = hmac.new(secret, b"GPUWorks rendezvous release" + token, hashlib.sha256).digest()
    path = os.path.join(tempfile.gettempdir(), "gpuworks-v1-" + identity.hex() + ".sock")
    if len(os.fsencode(path)) >= 100:
        raise RuntimeError("GPUWorks rendezvous socket path is too long")
    deadline = time.monotonic() + 600

    def remaining() -> float:
        left = deadline - time.monotonic()
        if left <= 0:
            raise TimeoutError(f"GPUWorks rendezvous timed out: {phase}")
        return left

    def receive_exactly(conn, size: int) -> bytes:
        chunks = []
        received = 0
        while received < size:
            conn.settimeout(remaining())
            chunk = conn.recv(size - received)
            if not chunk:
                raise RuntimeError(f"GPUWorks rendezvous closed during {phase}")
            chunks.append(chunk)
            received += len(chunk)
        return b"".join(chunks)

    def socket_identity():
        info = os.lstat(path)
        if not stat.S_ISSOCK(info.st_mode):
            raise RuntimeError(f"GPUWorks rendezvous path is not a UNIX socket: {path}")
        return info.st_dev, info.st_ino

    def unlink_socket_if(expected) -> None:
        try:
            info = os.lstat(path)
        except FileNotFoundError:
            return
        if stat.S_ISSOCK(info.st_mode) and (info.st_dev, info.st_ino) == expected:
            os.unlink(path)

    def remove_stale_socket() -> None:
        try:
            expected = socket_identity()
        except FileNotFoundError:
            return
        probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            probe.settimeout(0.2)
            probe.connect(path)
        except ConnectionRefusedError:
            unlink_socket_if(expected)
        except FileNotFoundError:
            pass
        else:
            raise RuntimeError(f"GPUWorks rendezvous socket is already in use: {path}")
        finally:
            probe.close()

    if rank == 0:
        peers = {}
        owned = None
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            # Never unlink a regular file, directory, symlink or another live
            # rank zero's socket. A refused stale socket keeps its inode check.
            remove_stale_socket()
            server.bind(path)
            owned = socket_identity()
            server.listen(world_size - 1)
            while len(peers) < world_size - 1:
                server.settimeout(remaining())
                conn, _ = server.accept()
                try:
                    peer_rank = int.from_bytes(receive_exactly(conn, 4), "big")
                    candidate = receive_exactly(conn, len(token))
                    expected_rank = 0 < peer_rank < world_size and peer_rank not in peers
                    if not expected_rank or not hmac.compare_digest(candidate, token):
                        conn.close()
                        continue
                    peers[peer_rank] = conn
                except Exception:
                    conn.close()
            for conn in peers.values():
                conn.settimeout(remaining())
                conn.sendall(release)
        finally:
            for conn in peers.values():
                conn.close()
            server.close()
            if owned is not None:
                unlink_socket_if(owned)
    else:
        client = None
        while client is None:
            remaining()
            candidate = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                candidate.settimeout(min(2.0, remaining()))
                candidate.connect(path)
                client = candidate
            except OSError:
                candidate.close()
                time.sleep(0.1)
        with client:
            client.settimeout(remaining())
            client.sendall(rank.to_bytes(4, "big") + token)
            response = receive_exactly(client, len(release))
            if not hmac.compare_digest(response, release):
                raise RuntimeError(f"GPUWorks rendezvous token mismatch: {phase}")
    print(f"GPUWorks: rank {rank} completed {phase} rendezvous", flush=True)


def _gpuworks_npu_fill_workaround() -> None:
    if getattr(torch.zeros, "_gpuworks_cpu_fill", False):
        return

    def is_npu(device) -> bool:
        if device is None:
            return False
        try:
            return torch.device(device).type == "npu"
        except (RuntimeError, TypeError):
            return False

    def fill_factory(original):
        def factory(*args, **kwargs):
            device = kwargs.get("device")
            if not is_npu(device) or kwargs.get("out") is not None:
                return original(*args, **kwargs)
            cpu_kwargs = dict(kwargs)
            cpu_kwargs["device"] = "cpu"
            cpu_tensor = original(*args, **cpu_kwargs)
            return cpu_tensor.to(device)

        factory._gpuworks_cpu_fill = True
        return factory

    torch.zeros = fill_factory(torch.zeros)
    torch.ones = fill_factory(torch.ones)
    torch.zeros_like = fill_factory(torch.zeros_like)
    torch.ones_like = fill_factory(torch.ones_like)
    torch.full = fill_factory(torch.full)
    torch.full_like = fill_factory(torch.full_like)
    print("GPUWorks: installed NPU zeros/ones CPU-copy fallback", flush=True)


def _gpuworks_coordinate_new_groups(key: str, rank: int, world_size: int):
    # rank and world_size are the identity of the group that owns key (its
    # distributed_init_method), the same one the phase rendezvous uses. The
    # global torch.distributed rank and size would make each data-parallel
    # group wait for ranks that belong to other groups.
    original = torch.distributed.new_group
    if getattr(original, "_gpuworks_coordinated", False):
        return original
    state = {"sequence": 0}

    def coordinated(*args, **kwargs):
        sequence = state["sequence"]
        state["sequence"] += 1
        backend = kwargs.get("backend", "default")
        _gpuworks_process_rendezvous(rank, world_size, key, f"new-group-{sequence}-enter-{backend}")
        group = original(*args, **kwargs)
        _gpuworks_process_rendezvous(rank, world_size, key, f"new-group-{sequence}-exit-{backend}")
        return group

    coordinated._gpuworks_coordinated = True
    torch.distributed.new_group = coordinated
    return original


if "_gpuworks_faulthandler_registered" not in globals():
    import faulthandler as _gpuworks_faulthandler
    import signal as _gpuworks_signal

    _gpuworks_faulthandler.register(_gpuworks_signal.SIGUSR1, all_threads=True)
    _gpuworks_faulthandler_registered = True

_gpuworks_npu_fill_workaround()
'''
method_before = "    def _init_worker_distributed_environment(self) -> None:\n"
method_after = '''    def _gpuworks_cpu_rendezvous(self, phase: str) -> None:
        _gpuworks_process_rendezvous(
            self.rank,
            self.parallel_config.world_size,
            self.distributed_init_method,
            phase,
        )

    def _init_worker_distributed_environment(self) -> None:
'''
init_before = (
    "        init_distributed_environment(\n"
    "            self.parallel_config.world_size, self.rank, self.distributed_init_method, self.local_rank, \"hccl\"\n"
    "        )"
)
init_after = (
    init_before
    + "\n        print(f\"GPUWorks: rank {self.rank} distributed environment initialized\", flush=True)"
)
worker_init_before = (
    "        self._init_worker_distributed_environment()\n"
    "        # Set random seed.\n"
    "        set_random_seed(self.model_config.seed)\n"
    "        # Initialize device properties used by triton kernels.\n"
    "        init_device_properties_triton()\n"
    "\n"
    "        return device"
)
worker_init_after = (
    "        print(f\"GPUWorks: rank {self.rank} initializing distributed environment\", flush=True)\n"
    "        self._init_worker_distributed_environment()\n"
    "        print(f\"GPUWorks: rank {self.rank} setting random seed\", flush=True)\n"
    "        set_random_seed(self.model_config.seed)\n"
    "        print(f\"GPUWorks: rank {self.rank} detecting Triton device properties\", flush=True)\n"
    "        init_device_properties_triton()\n"
    "        print(f\"GPUWorks: rank {self.rank} device preparation complete\", flush=True)\n"
    "\n"
    "        return device"
)
device_before = "        self.device = self._init_device()\n"
device_after = (
    "        print(f\"GPUWorks: rank {self.rank} initializing workspace\", flush=True)\n"
    "        self.device = self._init_device()\n"
    "        print(f\"GPUWorks: rank {self.rank} workspace initialization starting\", flush=True)\n"
)
model_runner_before = (
    "        # Init ModelRunner here, so that we have access to self.device.\n"
    "        if self.use_v2_model_runner:"
)
model_runner_after = (
    "        print(f\"GPUWorks: rank {self.rank} initializing model runner\", flush=True)\n"
    "        # Init ModelRunner here, so that we have access to self.device.\n"
    "        if self.use_v2_model_runner:"
)
phase_before = (
    "        init_ascend_model_parallel(self.parallel_config, self.vllm_config.engram_config)\n"
    "        ensure_ec_transfer_initialized(self.vllm_config)"
)
phase_after = (
    "        print(f\"GPUWorks: rank {self.rank} upstream groups initialized\", flush=True)\n"
    "        self._gpuworks_cpu_rendezvous(\"upstream-groups\")\n"
    "        _gpuworks_original_new_group = _gpuworks_coordinate_new_groups(\n"
    "            self.distributed_init_method, self.rank, self.parallel_config.world_size\n"
    "        )\n"
    "        try:\n"
    "            init_ascend_model_parallel(self.parallel_config, self.vllm_config.engram_config)\n"
    "        finally:\n"
    "            torch.distributed.new_group = _gpuworks_original_new_group\n"
    "        print(f\"GPUWorks: rank {self.rank} Ascend groups initialized\", flush=True)\n"
    "        self._gpuworks_cpu_rendezvous(\"ascend-groups\")\n"
    "        print(f\"GPUWorks: rank {self.rank} distributed initialization complete\", flush=True)\n"
    "        ensure_ec_transfer_initialized(self.vllm_config)\n"
    "        print(f\"GPUWorks: rank {self.rank} worker distributed initialization returned\", flush=True)"
)
changed = False
if "def _gpuworks_process_rendezvous(" not in source:
    if handler_before not in source:
        raise SystemExit("GPUWorks: unsupported worker imports for rendezvous")
    source = source.replace(handler_before, handler_after, 1)
    changed = True
if "def _gpuworks_cpu_rendezvous(self, phase: str)" not in source:
    if method_before not in source:
        raise SystemExit("GPUWorks: unsupported worker class layout")
    source = source.replace(method_before, method_after, 1)
    changed = True
if init_before in source and "distributed environment initialized" not in source:
    source = source.replace(init_before, init_after, 1)
    changed = True
elif "distributed environment initialized" not in source:
    raise SystemExit("GPUWorks: unsupported distributed environment layout")
if worker_init_before in source:
    source = source.replace(worker_init_before, worker_init_after, 1)
    changed = True
elif "device preparation complete" not in source:
    raise SystemExit("GPUWorks: unsupported worker device-preparation layout")
if device_before in source:
    source = source.replace(device_before, device_after, 1)
    changed = True
elif "initializing workspace" not in source:
    raise SystemExit("GPUWorks: unsupported worker workspace layout")
if model_runner_before in source:
    source = source.replace(model_runner_before, model_runner_after, 1)
    changed = True
elif "initializing model runner" not in source:
    raise SystemExit("GPUWorks: unsupported model-runner initialization layout")
if phase_before in source:
    source = source.replace(phase_before, phase_after, 1)
    changed = True
elif (
    "self._gpuworks_cpu_rendezvous(\"upstream-groups\")" not in source
    or "self.distributed_init_method, self.rank, self.parallel_config.world_size" not in source
):
    raise SystemExit("GPUWorks: unsupported distributed phase layout")
if changed:
    worker.write_text(source, encoding="utf-8")
    print("GPUWorks: synchronized distributed group initialization phases")
PY
python3 - <<'PY'
from pathlib import Path

torch_utils = Path("/vllm-workspace/vllm/vllm/utils/torch_utils.py")
source = torch_utils.read_text(encoding="utf-8")
before = "        current_platform.manual_seed_all(seed)"
after = (
    "        if hasattr(torch, \"npu\"):\n"
    "            torch.npu.manual_seed(seed)\n"
    "        else:\n"
    "            current_platform.manual_seed_all(seed)"
)
if after not in source:
    if before not in source:
        raise SystemExit("GPUWorks: unsupported vLLM random-seed layout")
    torch_utils.write_text(source.replace(before, after, 1), encoding="utf-8")
    print("GPUWorks: seed the current NPU device instead of every visible device")
PY
