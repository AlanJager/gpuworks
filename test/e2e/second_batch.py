#!/usr/bin/env python3
"""Run the seven model-acquisition contracts in en/zh without retries.

Run: make test-e2e-second-batch E2E_SECOND_BATCH_DIR=secrets/e2e/second-batch-final
Requires an explicitly selected private Docker daemon, real agent binaries,
cached fake-engine image and Chrome. Source, target and binary drift fail closed.
The batch file indexes individual reports; it does not replace their evidence.
"""
from first_batch import main

CASES = (
    ("download-delete", "model_files_lifecycle.py", "model-files-download-delete.json", ("--case", "download-delete")),
    ("failure-retry", "model_files_lifecycle.py", "model-files-failure-retry.json", ("--case", "failure-retry")),
    ("interrupted-recovery", "model_files_lifecycle.py", "model-files-interrupted-recovery.json", ("--case", "interrupted-recovery")),
    *[(f"{engine}-{provider}", "deployment_acquisition.py", f"deployment-acquisition-{engine}-{provider}.json",
       ("--case", f"{engine}-{provider}"))
      for engine in ("vllm", "sglang") for provider in ("huggingface", "modelscope")],
)

if __name__ == "__main__":
    raise SystemExit(main(cases=CASES, kind="second-batch-index", description=__doc__))
