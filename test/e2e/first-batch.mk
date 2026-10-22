# Explicit browser batch; not part of offline make test. Cache images on the
# dedicated daemon first. E2E_BATCH_DIR must be a fresh ignored artifact folder.
.PHONY: test-e2e-first-batch test-e2e-first-batch-fixtures
E2E_BATCH_DIR ?= secrets/e2e/first-batch-final
export E2E_BATCH_DIR
test-e2e-first-batch: build test-e2e-first-batch-fixtures
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 -B test/e2e/first_batch.py --artifact-dir "$${E2E_BATCH_DIR}"

test-e2e-first-batch-fixtures: test-e2e-deployment-health-fixture test-e2e-model-hub-fixture test-e2e-instance-diagnostics-fixture test-e2e-container-terminal-fixture
	PYTHONPATH=test/e2e python3 -B -m unittest test_local_business_runner test_first_batch -v
