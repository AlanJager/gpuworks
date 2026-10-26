#!/usr/bin/env python3
"""Mainline: exact public download/delete, same-row Retry, automatic Range resume.

Run after make build with a dedicated DOCKER_HOST (shared default refused):
  python3 -B test/e2e/model_files_lifecycle.py --case download-delete --locale en
Cases: download-delete, failure-retry, interrupted-recovery, independently en/zh.
Requires real server/agent, Playwright/Chrome and the established dedicated
Docker preflight image. No GPU inference is claimed. Private artifacts are
retained under secrets/e2e/<runId> for seven days; only owned resources stop.
"""
from pathlib import Path

from local_business_runner import case_parser, run_case
from model_files_fixture import cleanup, prepare
from model_files_browser import (exercise_download_delete, exercise_failure_retry,
                                 exercise_interrupted_recovery)

CASES = {'download-delete': exercise_download_delete,
         'failure-retry': exercise_failure_retry,
         'interrupted-recovery': exercise_interrupted_recovery}
HELPERS = ('test/e2e/model_files_fixture.py', 'test/e2e/model_download_fixture.py',
           'test/e2e/platform_token_read_revoke.py', 'test/e2e/test_model_files_fixture.py',
           'test/e2e/test_model_files_runner.py', 'test/e2e/test_model_download_fixture.py',
           'test/e2e/model_file_delete_blocks.py')


def main():
    args = case_parser(__doc__, CASES).parse_args()
    return run_case(args, contract_id='gpuworks/models/files-' + args.case,
        manifest='test/e2e/coverage/model-files-' + args.case + '.json',
        test_file=Path(__file__), prepare=prepare, exercise=CASES[args.case],
        cleanup=cleanup, helpers=HELPERS)


if __name__ == '__main__':
    raise SystemExit(main())
