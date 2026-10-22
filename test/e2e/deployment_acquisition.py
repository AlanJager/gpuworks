#!/usr/bin/env python3
"""Real public download → automatic placement → container execution.

Run: python3 -B test/e2e/deployment_acquisition.py --case vllm-huggingface --locale en
Cases: vllm-huggingface/vllm-modelscope/sglang-huggingface/sglang-modelscope.
Requires built binaries, Python Playwright/Chrome, a dedicated Docker daemon with
fake-engine:test. No cloud/GPU inference. Each run is isolated and never retries.
"""
from functools import partial
from local_business_runner import case_parser, run_case
import deployment_acquisition_fixture as fixture
from deployment_acquisition_browser import exercise


def main(argv=None):
    args = case_parser(__doc__, fixture.CASES).parse_args(argv)
    return run_case(args, contract_id='gpuworks/deployment/acquisition-' + args.case,
        manifest='test/e2e/coverage/deployment-acquisition-' + args.case + '.json',
        test_file='test/e2e/deployment_acquisition.py',
        prepare=partial(fixture.prepare, case=args.case), exercise=exercise, cleanup=fixture.cleanup,
        helpers=('test/e2e/deployment_acquisition_fixture.py',
                 'test/e2e/deployment_acquisition_oracles.py',
                 'test/e2e/model_download_fixture.py',
                 'test/e2e/deployment_container_fixture.py',
                 'test/e2e/deployment_sleep_wake.py', 'test/e2e/deployment_wizard_create.py',
                 'test/e2e/platform_token_read_revoke.py'), image=fixture.IMAGE)


if __name__ == '__main__':
    raise SystemExit(main())
