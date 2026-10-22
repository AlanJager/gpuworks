# Exactly seven contracts, each with its own en/zh evidence and observed cleanup.
.PHONY: test-e2e-second-batch test-e2e-second-batch-fixtures
E2E_SECOND_BATCH_DIR ?= secrets/e2e/second-batch-final
export E2E_SECOND_BATCH_DIR

test-e2e-second-batch: build test-e2e-second-batch-fixtures
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 -B test/e2e/second_batch.py --artifact-dir "$${E2E_SECOND_BATCH_DIR}"

test-e2e-second-batch-fixtures: test-e2e-model-files-fixtures
	python3 -B -m unittest discover -s test/e2e -p 'test_deployment_acquisition_*.py'
	python3 -B -m unittest discover -s test/e2e -p 'test_*batch.py'
