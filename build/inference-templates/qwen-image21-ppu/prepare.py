"""Install the Qwen-Image-2.1 modules into this instance of the PPU runner."""
import hashlib
import os
from pathlib import Path
import sysconfig
import tempfile

HERE = Path(__file__).resolve().parent
RUNNER = "hggc13.0-sglang0.5.13-r3"
DIFFUSERS = Path(sysconfig.get_path("purelib")) / "diffusers"
SGLANG = Path(sysconfig.get_path("purelib")) / "sglang/multimodal_gen"

MODEL_EXPORTS = {
    "autoencoders.autoencoder_kl_qwenimage21": ["AutoencoderKLQwenImage21"],
    "transformers.transformer_qwenimage21": ["QwenImage21Transformer2DModel"],
}
ROOT_EXPORTS = {"models." + module: names for module, names in MODEL_EXPORTS.items()}
ROOT_EXPORTS["pipelines.qwenimage21.pipeline_qwenimage21"] = ["QwenImage21Pipeline"]
LAZY_MODULE = "    sys.modules[__name__] = _LazyModule("


def replace(path, old, new, original_hash):
    if not path.is_file():
        raise ValueError(f"Qwen-Image-2.1 Diffusers patch requires runner {RUNNER}; missing module {path}")
    data = path.read_bytes()
    old, new = old.encode(), new.encode()
    if data.count(new) == 1:
        if hashlib.sha256(data.replace(new, old)).hexdigest() == original_hash:
            return
    actual = hashlib.sha256(data).hexdigest()
    if actual != original_hash or data.count(old) != 1:
        raise ValueError(f"Qwen-Image-2.1 Diffusers patch requires runner {RUNNER}; incompatible {path} "
                         f"(expected SHA-256 {original_hash}, found {actual})")
    write(path, data.replace(old, new))


def write(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, filename = tempfile.mkstemp(dir=path.parent)
    temporary = Path(filename)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(data)
        temporary.chmod(0o644)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def prepare():
    for src in sorted((HERE / "diffusers").rglob("*.py")):
        dest = DIFFUSERS / src.relative_to(HERE / "diffusers")
        data = src.read_bytes()
        if dest.exists():
            if dest.read_bytes() != data:
                raise ValueError(f"Qwen-Image-2.1 Diffusers patch requires runner {RUNNER}; conflicting module {dest}")
        else:
            write(dest, data)
    for path, exports, digest in (
        (DIFFUSERS / "__init__.py", ROOT_EXPORTS, "b027dc590f9790838bf60069b501050904d502616da4b5566098c1b1185cdd12"),
        (DIFFUSERS / "models/__init__.py", MODEL_EXPORTS, "3a7e1c7b025262c55d5e86b998776ab923b61feecdd4802bfe16ca1caafe46fa"),
    ):
        replace(path, LAZY_MODULE, f"    _import_structure.update({exports!r})\n" + LAZY_MODULE, digest)
    # A module-level config can be reconstructed by SGLang's spawned workers.
    end = "        return num_frames\n"
    edit_config = '''

@dataclass
class DiffusersTI2IPipelineConfig(DiffusersGenericPipelineConfig):
    task_type: ModelTaskType = ModelTaskType.TI2I
'''
    replace(SGLANG / "configs/pipeline_configs/diffusers_generic.py", end, end + edit_config,
            "89f8a2961a3155938da458eebd7c81bb2b6dd115c622b0e6b757a9ab4a5c9014")
    result = "    return ModelInfo(\n        pipeline_cls=DiffusersPipeline,"
    edit = '''    if model_path and os.path.isfile(os.path.join(model_path, "model_index.json")):
        import json
        with open(os.path.join(model_path, "model_index.json"), encoding="utf-8") as index:
            if json.load(index).get("_class_name") == "QwenImage21Pipeline":
                from sglang.multimodal_gen.configs.pipeline_configs.diffusers_generic import DiffusersTI2IPipelineConfig
                pipeline_config_cls = DiffusersTI2IPipelineConfig
'''
    replace(SGLANG / "registry.py", result, edit + result,
            "c94c8db619ed37027e71e89593b0d5f3b42ac324c578b2cbfdacf0c6639d82fd")
    fmt = '    fmt = (output_format or "").lower()'
    replace(SGLANG / "runtime/entrypoints/openai/utils.py", fmt,
            '    if not output_format:\n        return "png"\n' + fmt,
            "dbb8bc79e47f6a315ec02adb1cbcb4c296f7780ba8f6bf6009a2924480fbd385")


if __name__ == "__main__":
    prepare()
