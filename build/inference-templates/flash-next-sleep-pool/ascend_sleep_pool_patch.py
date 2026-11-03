"""Scope the vLLM-Ascend level-1 sleep kv_cache pool to the KV backing allocation.

The inference template runs this inside the engine container, before vLLM is
imported, for the 910C (A3) Qwen3.8-Flash-Next recipe.

The vllm-ascend build in this recipe's image wraps the whole
``initialize_kv_cache()`` call in the discardable ``kv_cache`` memory pool.
Every persistent runtime tensor built in that scope -- attention metadata
builders, block tables, graph-safe constants -- inherits the ``kv_cache`` tag,
so a level-1 sleep frees its storage while keeping the same virtual address.
Afterwards ``/sleep`` and ``/wake_up`` both still report success, but the first
real forward pass reads freed or reused device memory. On this model's GDN
hybrid layers every worker then aborts at once inside
``gdn_attn_builder._build_non_spec_chunked_prefill_metadata``:

    RuntimeError: SUSPECT REMOTE ERROR, ERR00100 PTA call acl api failed

This patch moves the pool down to the backing KV allocation only, which is the
upstream fix (vllm-project/vllm-ascend PR #16096, merged 2026-09-16 -- newer
than the vllm-ascend build in this image).

The patch is bounded on purpose: it edits only the sources whose SHA-256
digests are pinned below, pins the patched outputs as well (so a container
restart re-runs it as a no-op and refuses a hand-edited file), and otherwise
fails the container start loudly instead of running a half-patched engine.
Model runners it does not cover (MRV2, 310P, xlite) keep their current
behaviour: the patched worker checks the runner's signature rather than pass a
keyword argument the runner may not accept.
"""
import ast
import hashlib
import importlib.util
import os
from pathlib import Path
import shutil
import tempfile

MARKER = "# GPUWorks: sleep-mode kv_cache pool scoped to the KV allocation, version 1\n"

# vllm-ascend 0.1.dev5+g550ab66e7 (2026-09-11), the build shipped in
# quay.io/ascend/vllm-ascend@sha256:2cbb274af050cb5be7db7318e1c461fe22ec0643c54f1ed32aa7266513a7d421
# (upstream tag qwen3.8-next-a3).
SOURCES = {
    'worker/worker.py': {'eb0bb11de6dffecbed135979655cf032cccef3f3ea4fe3b93254933d47e0f8c0'},
    'worker/model_runner_v1.py': {'de9f108bb1365c7eb5967c0cd098a005510fe627f7a27e0732d120a0f33e2608'},
}

# The same files after this patch, so a re-run is a no-op and a hand edit is refused.
PATCHED = {
    'worker/worker.py': {'e03b8c73e0fd859f77076be0538a8b8ec05385b05631a924e795e6d7896a0e29'},
    'worker/model_runner_v1.py': {'0189c4e73dca9fe500e2ef9c5b916daeff986ddc1bb7a0e556744ce2442a63d0'},
}

# worker.py: stop wrapping the whole initialize_kv_cache() in the pool;
# pass it down instead, and dedent the eagle3 tail out of the removed `with`.
WORKER_INITIALIZE_FROM_CONFIG = (
    '''\
    def initialize_from_config(self, kv_cache_config: KVCacheConfig) -> None:
        """Allocate NPU KV cache with the specified kv_cache_config."""
        ensure_kv_transfer_initialized(self.vllm_config, kv_cache_config)
        if self.vllm_config.model_config.enable_sleep_mode:
            allocator = CaMemAllocator.get_instance()
            context = allocator.use_memory_pool(tag="kv_cache")
        else:
            from contextlib import nullcontext

            context = nullcontext()  # type: ignore
        with context:
            self.model_runner.initialize_kv_cache(kv_cache_config)

            # Restrict to mamba and full attn hybrid models (e.g. Qwen3.x).
            #
            # When eagle3 is enabled with num_speculative_tokens>1, mamba blocks may be reallocated to full blocks if
            # the target and draft models share the same kv cache tensor (e.g. unaligned full attn layers with
            # different num_kv_heads and head_size). In addition, for performance reasons, the current mtp/eagle path
            # does not update seq_lens_cpu with num_rejected_tokens for step>1, since it would require d2h sync. As a
            # result, seq_lens_cpu can become stale and some blocks will be unintentionally used.
            #
            # If an uncleared mamba block is later reused, the stale state combined with the incorrect seq_lens_cpu may
            # lead to NaNs and reduced acceptance rate.
            if (
                kv_cache_config.needs_kv_cache_zeroing
                and hasattr(self.model_runner, "_init_kv_zero_meta")
                and self.vllm_config is not None
                and self.vllm_config.speculative_config is not None
                and self.vllm_config.speculative_config.method == "eagle3"
                and self.vllm_config.speculative_config.num_speculative_tokens > 1
            ):
                self.model_runner._init_kv_zero_meta()
''',
    '''\
    def _maybe_get_memory_pool_context(self, tag: str):
        """Return the sleep-mode pool for ``tag``, or a no-op context."""
        from contextlib import nullcontext

        if self.vllm_config.model_config.enable_sleep_mode:
            allocator = CaMemAllocator.get_instance()
            return allocator.use_memory_pool(tag=tag)
        return nullcontext()

    def initialize_from_config(self, kv_cache_config: KVCacheConfig) -> None:
        """Allocate NPU KV cache with the specified kv_cache_config."""
        import inspect

        ensure_kv_transfer_initialized(self.vllm_config, kv_cache_config)
        # Keep the discardable kv_cache pool around the backing KV allocation
        # only. Persistent metadata built during initialize_kv_cache (attention
        # metadata builders, block tables, graph-safe constants) must stay
        # outside it: level-1 sleep frees whatever the pool holds while keeping
        # the virtual address, so the first forward pass after wake-up reads
        # freed memory and the device aborts the stream.
        pool = self._maybe_get_memory_pool_context(tag="kv_cache")
        if "kv_cache_allocation_context" in inspect.signature(
            self.model_runner.initialize_kv_cache
        ).parameters:
            self.model_runner.initialize_kv_cache(
                kv_cache_config, kv_cache_allocation_context=pool
            )
        else:
            # A model runner this patch does not cover (MRV2, 310P, xlite)
            # keeps its current behaviour instead of failing on an unexpected
            # keyword argument.
            with pool:
                self.model_runner.initialize_kv_cache(kv_cache_config)

        # Restrict to mamba and full attn hybrid models (e.g. Qwen3.x).
        #
        # When eagle3 is enabled with num_speculative_tokens>1, mamba blocks may be reallocated to full blocks if
        # the target and draft models share the same kv cache tensor (e.g. unaligned full attn layers with
        # different num_kv_heads and head_size). In addition, for performance reasons, the current mtp/eagle path
        # does not update seq_lens_cpu with num_rejected_tokens for step>1, since it would require d2h sync. As a
        # result, seq_lens_cpu can become stale and some blocks will be unintentionally used.
        #
        # If an uncleared mamba block is later reused, the stale state combined with the incorrect seq_lens_cpu may
        # lead to NaNs and reduced acceptance rate.
        if (
            kv_cache_config.needs_kv_cache_zeroing
            and hasattr(self.model_runner, "_init_kv_zero_meta")
            and self.vllm_config is not None
            and self.vllm_config.speculative_config is not None
            and self.vllm_config.speculative_config.method == "eagle3"
            and self.vllm_config.speculative_config.num_speculative_tokens > 1
        ):
            self.model_runner._init_kv_zero_meta()
''',
)

# model_runner_v1.py: accept the pool and forward it.
RUNNER_INITIALIZE_KV_CACHE_SIGNATURE = (
    '''\
    def initialize_kv_cache(self, kv_cache_config: KVCacheConfig) -> None:
        """
        Initialize KV cache based on `kv_cache_config`.
        Args:
            kv_cache_config: Configuration for the KV cache, including the KV
            cache size of each layer
        """
''',
    '''\
    def initialize_kv_cache(
        self,
        kv_cache_config: KVCacheConfig,
        kv_cache_allocation_context=None,
    ) -> None:
        """
        Initialize KV cache based on `kv_cache_config`.
        Args:
            kv_cache_config: Configuration for the KV cache, including the KV
            cache size of each layer
            kv_cache_allocation_context: Sleep-mode pool entered only around the
            discardable KV backing allocation.
        """
''',
)

# model_runner_v1.py: hand the pool to the tensor allocation step.
RUNNER_FORWARD_ALLOCATION_CONTEXT = (
    '''\
        kv_caches = self.initialize_kv_cache_tensors(kv_cache_config)
''',
    '''\
        kv_caches = self.initialize_kv_cache_tensors(
            kv_cache_config,
            kv_cache_allocation_context=kv_cache_allocation_context,
        )
''',
)

# model_runner_v1.py: enter the pool only around the backing allocation;
# reshape, cross-layer sharing and bind stay outside it.
RUNNER_ALLOCATION_SCOPE = (
    '''\
    def initialize_kv_cache_tensors(self, kv_cache_config: KVCacheConfig) -> dict[str, torch.Tensor]:
        """
        Initialize the memory buffer for KV cache.

        Args:
            kv_cache_config: The KV cache config
        Returns:
            Dict[str, torch.Tensor]: A map between layer names to their
            corresponding memory buffer for KV cache.
        """
        # Initialize the memory buffer for KV cache
        kv_cache_raw_tensors = self._allocate_kv_cache_tensors(kv_cache_config)
''',
    '''\
    def initialize_kv_cache_tensors(
        self,
        kv_cache_config: KVCacheConfig,
        kv_cache_allocation_context=None,
    ) -> dict[str, torch.Tensor]:
        """
        Initialize the memory buffer for KV cache.

        Args:
            kv_cache_config: The KV cache config
            kv_cache_allocation_context: Sleep-mode pool entered only around the
            discardable KV backing allocation; reshape, cross-layer sharing and
            bind stay outside it.
        Returns:
            Dict[str, torch.Tensor]: A map between layer names to their
            corresponding memory buffer for KV cache.
        """
        # Only the backing allocation is discardable on level-1 sleep.
        with kv_cache_allocation_context or nullcontext():
            kv_cache_raw_tensors = self._allocate_kv_cache_tensors(kv_cache_config)
''',
)

EDITS = {
    'worker/worker.py': [WORKER_INITIALIZE_FROM_CONFIG],
    'worker/model_runner_v1.py': [
        RUNNER_INITIALIZE_KV_CACHE_SIGNATURE,
        RUNNER_FORWARD_ALLOCATION_CONTEXT,
        RUNNER_ALLOCATION_SCOPE,
    ],
}


def replace_once(text, old, new, relative):
    count = text.count(old)
    if count != 1:
        raise SystemExit(
            f"sleep-pool patch: context for {relative} matched {count} times, expected 1"
        )
    return text.replace(old, new, 1)


def write_atomic(path, data):
    with tempfile.NamedTemporaryFile(dir=str(path.parent), delete=False) as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
        temporary = Path(handle.name)
    try:
        shutil.copymode(str(path), str(temporary))
        os.replace(str(temporary), str(path))
    except BaseException:
        temporary.unlink(missing_ok=True)
        raise


def patch_root(root):
    """Patch the vllm_ascend package at root, or fail without writing a file."""
    root = Path(root)
    for relative, edits in EDITS.items():
        path = root / relative
        if not path.is_file():
            raise SystemExit(f"sleep-pool patch: {path} is missing")
        raw = path.read_bytes()
        digest = hashlib.sha256(raw).hexdigest()
        if digest in PATCHED[relative]:
            print(f"sleep-pool patch: {relative} already patched")
            continue
        if digest not in SOURCES[relative]:
            raise SystemExit(
                f"sleep-pool patch: unsupported source {relative}: sha256={digest}"
            )
        text = raw.decode()
        for old, new in edits:
            text = replace_once(text, old, new, relative)
        text = MARKER + text
        ast.parse(text, filename=str(path))
        data = text.encode()
        result = hashlib.sha256(data).hexdigest()
        if result not in PATCHED[relative]:
            raise SystemExit(
                f"sleep-pool patch: patched {relative} digest {result} is not pinned"
            )
        write_atomic(path, data)
        print(f"sleep-pool patch: patched {relative} -> sha256={result}")


if __name__ == "__main__":
    spec = importlib.util.find_spec("vllm_ascend")
    if spec is None or not spec.origin:
        raise SystemExit("sleep-pool patch requires the catalog vLLM-Ascend image")
    patch_root(Path(spec.origin).parent)
