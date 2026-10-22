"""Apply the bounded SDXL VAE fix to the pinned Omni 0.26/0.28 sources.
Refuse source drift instead of silently publishing an unpatched image.
"""
import importlib.util
import re
from pathlib import Path


def patch(source):
    anchor = '''        ).to(self.device)

        self.vae_scale_factor = 8'''
    replacement = '''        ).to(self.device)
        # SDXL declares force_upcast because its VAE can overflow in FP16.
        # Keep the denoiser in its selected dtype; decode latents already cast
        # to self.vae.dtype below. This also applies to Ascend.
        if self.vae.dtype == torch.float16 and self.vae.config.force_upcast:
            self.vae.to(dtype=torch.float32)

        self.vae_scale_factor = 8'''
    if source.count(anchor) != 1:
        raise ValueError('pinned SDXL VAE source changed; review patch before building')
    if 'latents = latents.to(self.vae.dtype)' not in source:
        raise ValueError('SDXL decode no longer casts latents to the VAE dtype')
    source = source.replace(anchor, replacement)
    scheduler_import = "from diffusers.schedulers.scheduling_euler_discrete import EulerDiscreteScheduler"
    scheduler_start = "        self.scheduler = EulerDiscreteScheduler.from_pretrained("
    if source.count(scheduler_import) != 1 or source.count(scheduler_start) != 1:
        raise ValueError('pinned SDXL scheduler source changed; review patch before building')
    source = source.replace(scheduler_import, scheduler_import + "\nfrom diffusers.schedulers.scheduling_euler_ancestral_discrete import EulerAncestralDiscreteScheduler")
    # Turbo declares EulerAncestral; the upstream implementation hard-codes the
    # base SDXL scheduler. Allow only the two scheduler classes this recipe tests.
    source = source.replace(scheduler_start, """        scheduler_config = EulerDiscreteScheduler.load_config(
            model, subfolder="scheduler", local_files_only=local_files_only
        )
        scheduler_type = {
            "EulerDiscreteScheduler": EulerDiscreteScheduler,
            "EulerAncestralDiscreteScheduler": EulerAncestralDiscreteScheduler,
        }.get(scheduler_config.get("_class_name"))
        if scheduler_type is None:
            raise ValueError("unsupported scheduler in the tested SDXL recipe")
        self.scheduler = scheduler_type.from_pretrained(""")
    # Omni initializes pipelines under the accelerator's default-device context.
    # Scheduler construction uses NumPy and must create its coefficients on CPU.
    source, count = re.subn(
        r'(?m)^        self.scheduler = scheduler_type.from_pretrained\(\n[\s\S]*?^        \)',
        lambda match: '        with torch.device("cpu"):\n' + '\n'.join(
            '    ' + line for line in match.group().splitlines()), source)
    if count != 1:
        raise ValueError('pinned SDXL scheduler constructor changed')
    return source


if __name__ == '__main__':
    root = Path(importlib.util.find_spec('vllm_omni').origin).parent
    path = root / 'diffusion/models/sdxl/pipeline_sdxl.py'
    path.write_text(patch(path.read_text()))
