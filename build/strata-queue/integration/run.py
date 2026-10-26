"""Run explicit CPU contracts against a fresh pinned Strata image's real code."""
from pathlib import Path
import subprocess
import sys

HERE = Path(__file__).resolve().parent
for script in ('test_source_patch.py', 'test_queue_wire.py', 'test_queue_hardening.py'):
    subprocess.run([sys.executable, str(HERE / script)], check=True)
