set -eu
python3 - <<'PY'
from pathlib import Path

worker = Path("/vllm-workspace/vllm-ascend/vllm_ascend/worker/worker.py")
source = worker.read_text(encoding="utf-8")
handler_before = "import torch\n"
handler_after = '''import torch

def _gpuworks_npu_fill_workaround() -> None:
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
            return original(*args, **cpu_kwargs).to(device)

        factory._gpuworks_cpu_fill = True
        return factory

    torch.zeros = fill_factory(torch.zeros)
    torch.ones = fill_factory(torch.ones)
    torch.zeros_like = fill_factory(torch.zeros_like)
    torch.ones_like = fill_factory(torch.ones_like)
    torch.full = fill_factory(torch.full)
    torch.full_like = fill_factory(torch.full_like)
    print("GPUWorks: installed NPU zeros/ones CPU-copy fallback", flush=True)


if "_gpuworks_faulthandler_registered" not in globals():
    import faulthandler as _gpuworks_faulthandler
    import signal as _gpuworks_signal

    _gpuworks_faulthandler.register(_gpuworks_signal.SIGUSR1, all_threads=True)
    _gpuworks_faulthandler_registered = True

_gpuworks_npu_fill_workaround()
'''

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

changed = False
if "def _gpuworks_npu_fill_workaround(" not in source:
    if handler_before not in source:
        raise SystemExit("GPUWorks: unsupported vLLM-Ascend worker imports")
    source = source.replace(handler_before, handler_after, 1)
    changed = True
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
if changed:
    worker.write_text(source, encoding="utf-8")
PY
