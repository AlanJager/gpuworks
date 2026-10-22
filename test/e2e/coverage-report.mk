# Offline E2E business/component coverage; no server, browser or test execution.
# E2E_REPORT_DIR may point to an earlier worktree's secrets/e2e directory.
.PHONY: test-e2e-coverage test-e2e-coverage-check
export E2E_REPORT_DIR E2E_COVERAGE_OUTPUT
test-e2e-coverage: test-e2e-coverage-check
	python3 -B scripts/e2e_coverage.py

test-e2e-coverage-check:
	python3 -B -m unittest discover -s scripts -p 'test_e2e_coverage*.py'
