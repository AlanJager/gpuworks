# Entries for the standalone browser/server cases that take no arguments.
#
# Each of these has a __main__ guard, drives a real browser or an isolated
# server, and needs no CLI argument, so it is reachable as
# `make test-e2e-<name>` instead of a hand-written python3 invocation. Cases
# that take arguments are left out on purpose: guessing them would be worse
# than the status quo. Helpers, self-tests and fixtures are excluded.

.PHONY: test-e2e-benchmark-admission-walkthrough
test-e2e-benchmark-admission-walkthrough: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/benchmark_admission_walkthrough.py

.PHONY: test-e2e-benchmark-support
test-e2e-benchmark-support: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/benchmark_support.py

.PHONY: test-e2e-model-metadata-table-browser
test-e2e-model-metadata-table-browser: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/model_metadata_table_browser.py

.PHONY: test-e2e-terminal-host-gates
test-e2e-terminal-host-gates: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/terminal_host_gates.py
