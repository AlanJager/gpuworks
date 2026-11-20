"""Test the Ascend worker overlay without importing PyTorch or requiring an NPU."""

from contextlib import contextmanager
from contextvars import ContextVar
import json
import os
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
from typing import Any
import unittest


ROOT = Path(__file__).resolve().parent
# Worker method and hunk context from vLLM-Ascend v0.26.0rc1.
WORKER = """\
class NPUWorker:
    def update_config(self, overrides: dict[str, Any]) -> None:
        self.model_runner.update_config(overrides)

    def reload_weights(self, *args, **kwargs) -> None:
        self.model_runner.reload_weights(*args, **kwargs)

    def check_health(self) -> None:
        import subprocess
"""


class AscendReloadPatchTest(unittest.TestCase):
    def test_reload_sets_worker_config_and_restores_it_on_exception(self):
        config = object()
        previous = object()
        current = ContextVar("vllm_config", default=None)

        @contextmanager
        def set_current_vllm_config(value):
            token = current.set(value)
            try:
                yield
            finally:
                current.reset(token)

        def reload_model(path, *, is_checkpoint_format):
            if current.get() is not config:
                raise AssertionError("Current vLLM config is not set")
            self.assertEqual(path, "/model")
            if not is_checkpoint_format:
                raise ValueError("loader failed")

        def load_worker(source):
            env = {"Any": Any, "set_current_vllm_config": set_current_vllm_config}
            exec(compile(source, "worker.py", "exec"), env)
            worker = env["NPUWorker"]()
            worker.vllm_config = config
            worker.model_runner = SimpleNamespace(reload_weights=reload_model)
            return worker

        unpatched = load_worker(WORKER)
        with self.assertRaisesRegex(AssertionError, "Current vLLM config is not set"):
            unpatched.reload_weights("/model", is_checkpoint_format=True)

        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            worker_source = target / "vllm_ascend/worker/worker.py"
            worker_source.parent.mkdir(parents=True)
            worker_source.write_text(WORKER)
            manifest = target / "applied.json"
            subprocess.run(
                ["bash", str(ROOT / "shared/apply_patches.sh")],
                env=dict(os.environ, PATCH_DIR=str(ROOT / "patches/vllm_ascend"),
                         TARGET=str(target), MANIFEST=str(manifest), EXPECT_ALL="1",
                         NOT_APPLICABLE=""),
                check=True, capture_output=True, text=True, timeout=10,
            )
            self.assertEqual(json.loads(manifest.read_text()), {
                "applied": ["001_reload_weights_config.patch"], "failed": [], "skipped": [],
            })
            worker = load_worker(worker_source.read_text())

        worker.reload_weights("/model", is_checkpoint_format=True)
        self.assertIsNone(current.get())
        with set_current_vllm_config(previous):
            with self.assertRaisesRegex(ValueError, "loader failed"):
                worker.reload_weights("/model", is_checkpoint_format=False)
            self.assertIs(current.get(), previous)


if __name__ == "__main__":
    unittest.main()
