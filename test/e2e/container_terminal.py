#!/usr/bin/env python3
"""Container target PTY, release and permission contract; one locale per run.

Run: make test-e2e-container-terminal E2E_LOCALE=en
Requires the dedicated local Docker fixture and cached Alpine/fake-engine image.
"""
from local_business_runner import case_parser, run_case
from container_terminal_browser import test_container_terminal
from container_terminal_fixture import prepare, cleanup
from instance_diagnostics import HELPERS


def main():
    args = case_parser(__doc__).parse_args()
    return run_case(args, contract_id="gpuworks/terminal/container-pty-release",
        manifest="test/e2e/coverage/container-terminal.json", test_file=__file__,
        prepare=prepare, exercise=test_container_terminal, cleanup=cleanup,
        helpers=(*HELPERS, "test/e2e/container_terminal_fixture.py", "test/e2e/test_container_terminal.py",
                 "test/e2e/instance_diagnostics.py", "test/e2e/instance_diagnostics_browser.py"))


if __name__ == "__main__":
    raise SystemExit(main())
