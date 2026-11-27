"""Build the offline ZW810E adapter from a pinned Hugging Face Diffusers archive."""
import gzip
import hashlib
import io
from pathlib import Path
import sys
import tarfile

HERE = Path(__file__).resolve().parent
MODEL = HERE.parents[2] / "internal/server/inferencecatalog/seed/catalog/models/qwen/qwen-image-2.1"
REVISION = "d9586a3dc62cce5091bdb7cff981d725f4a6dfe5"
UPSTREAM_SHA256 = {
    "LICENSE": "f9e2070c247517b1ddf65f7b11b393484a18da91a958fd18a97bd0f241c3125c",
    "src/diffusers/models/autoencoders/autoencoder_kl_qwenimage21.py": "247672b153f32d48e42d7c28d3fc99e702bc568ac247b9c85d16cf8d2fda1a99",
    "src/diffusers/models/transformers/transformer_qwenimage21.py": "3a583a8d70f54d1926b7ce4a9109a098c8d9054d944197820aca29db4e1183e1",
    "src/diffusers/pipelines/qwenimage21/__init__.py": "75a9750e85e9b6b5910c5fa2da4fcd145aee08c1af6a49f2b8fb23ac5405b702",
    "src/diffusers/pipelines/qwenimage21/pipeline_qwenimage21.py": "7f27ee83572d40216ea47904575d66d50fbdef8aa055ce05e1754379e81ada10",
}


def build(source):
    files = {"prepare.py": (HERE / "prepare.py").read_text(encoding="utf-8").encode()}
    with tarfile.open(source, mode="r:gz") as upstream:
        for name, expected in UPSTREAM_SHA256.items():
            dest = "LICENSE.txt" if name == "LICENSE" else name.removeprefix("src/")
            content = upstream.extractfile(f"diffusers-{REVISION}/{name}").read()
            if hashlib.sha256(content).hexdigest() != expected:
                raise ValueError(f"SHA-256 mismatch for {name} at {REVISION}")
            if dest.endswith("/pipeline_qwenimage21.py"):
                # The runner's Diffusers 0.37 cache API takes only the context name.
                for context in ("cond", "uncond"):
                    old = f'cache_context("{context}", timestep=t)'.encode()
                    if content.count(old) != 1:
                        raise ValueError(f"unexpected {context} cache call in {dest}")
                    content = content.replace(old, f'cache_context("{context}")'.encode())
                content = b"# Modified by GPUWorks for the Diffusers 0.37 cache API.\n" + content
            files[dest] = content
    output = io.BytesIO()
    with gzip.GzipFile(fileobj=output, mode="wb", mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode="w", format=tarfile.USTAR_FORMAT) as tar:
            for name, content in sorted(files.items()):
                entry = tarfile.TarInfo(name)
                entry.mode, entry.size = 0o444, len(content)
                tar.addfile(entry, io.BytesIO(content))
    target = MODEL / "assets/qwenimage21-ppu.tar.gz"
    target.parent.mkdir(parents=True, exist_ok=True)
    data = output.getvalue()
    target.write_bytes(data)
    print(hashlib.sha256(data).hexdigest(), len(data), target.name)


if __name__ == "__main__":
    build(Path(sys.argv[1]))
