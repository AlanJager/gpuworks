"""Write the self-description consumed by runner-images/verify.sh."""
import json
import os
from pathlib import Path

Path("/opt/gpuworks/image.json").write_text(json.dumps({
    "id": os.environ["IMAGE_ID"],
    "revision": os.environ["IMAGE_REVISION"],
    "engine": os.environ["ENGINE"],
    "engine_version": os.environ["ENGINE_VERSION"],
    "framework": "rocm",
    "framework_version": os.environ["FRAMEWORK_VERSION"],
    "requires": os.environ["REQUIRES"].split(),
}, indent=2, sort_keys=True) + "\n")
