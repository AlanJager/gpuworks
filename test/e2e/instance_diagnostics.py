#!/usr/bin/env python3
"""Run the real instance diagnostics contract, one independent locale per run.

Run: make test-e2e-instance-diagnostics E2E_LOCALE=en
Requires dedicated DOCKER_HOST, cached alpine:3.20 and fake-engine:test, Chrome,
and current clean-tree server/agent binaries. No public image pulls or GPU work.
"""
from local_business_runner import case_parser, run_case
from instance_diagnostics_browser import test_instance_diagnostics
from instance_diagnostics_fixture import prepare, cleanup

HELPERS = ("test/e2e/instance_diagnostics_fixture.py", "test/e2e/deployment_override_fixture.py",
           "test/e2e/deployment_wizard_fixture.py", "test/e2e/deployment_container_fixture.py",
           "test/e2e/deployment_sleep_wake.py", "test/e2e/deployment_lifecycle.py",
           "test/e2e/platform_token_read_revoke.py", "test/fake-engine/main.go",
           "test/fake-engine/handler/metrics.go", "test/e2e/test_instance_diagnostics.py")


def main():
    args = case_parser(__doc__).parse_args()
    return run_case(args, contract_id="gpuworks/deployment/instance-diagnostics",
        manifest="test/e2e/coverage/instance-diagnostics.json", test_file=__file__,
        prepare=prepare, exercise=test_instance_diagnostics, cleanup=cleanup, helpers=HELPERS)


if __name__ == "__main__":
    raise SystemExit(main())
