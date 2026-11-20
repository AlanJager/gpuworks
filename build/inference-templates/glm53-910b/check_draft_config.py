"""Verify draft Eager isolation against the installed proposer and config classes."""
from dataclasses import dataclass
from types import SimpleNamespace


def check_draft_config():
    from vllm.config import CompilationConfig, CompilationMode, CUDAGraphMode
    from vllm_ascend.spec_decode.dflash_proposer import AscendDflashProposer

    @dataclass
    class Attention:
        backend: object = None

    @dataclass
    class ParentConfig:
        model_config: object = None
        compilation_config: object = None
        attention_config: object = None

    for eager in (False, True):
        model = SimpleNamespace(runner_type="generate", enforce_eager=False)
        compilation = CompilationConfig(cudagraph_mode=CUDAGraphMode.FULL_DECODE_ONLY)
        original_mode = compilation.mode
        parent = ParentConfig(model, compilation, Attention())
        # Exercise the real configuration hook without loading any weights.
        proposer = object.__new__(AscendDflashProposer)
        proposer.vllm_config = parent
        proposer.speculative_config = SimpleNamespace(
            moe_backend=None, attention_backend=None, kv_cache_dtype=None,
            enforce_eager=eager, draft_model_config=SimpleNamespace(runner_type="draft"),
        )
        draft = proposer._create_draft_vllm_config()
        assert draft is not parent and draft.model_config is not model
        assert draft.model_config.runner_type == "draft"
        assert model.runner_type == "generate" and not model.enforce_eager
        assert compilation.mode == original_mode
        assert compilation.cudagraph_mode == CUDAGraphMode.FULL_DECODE_ONLY
        if eager:
            assert draft.model_config.enforce_eager
            assert draft.compilation_config is not compilation
            assert draft.compilation_config.mode == CompilationMode.NONE
            assert draft.compilation_config.cudagraph_mode == CUDAGraphMode.NONE
        else:
            assert draft.compilation_config is compilation
    print("GLM_DRAFT_CONFIG_OK eager_no_compile target_graph_preserved", flush=True)


if __name__ == "__main__":
    check_draft_config()
