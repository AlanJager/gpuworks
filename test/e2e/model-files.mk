LOCALE ?= en
MODEL_FILES_CASE ?= download-delete
.PHONY: test-e2e-model-files test-e2e-model-files-fixtures
test-e2e-model-files: build test-e2e-model-files-fixtures
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 -B test/e2e/model_files_lifecycle.py --case $(MODEL_FILES_CASE) --locale $(LOCALE)
test-e2e-model-files-fixtures:
	python3 -B -m unittest discover -s test/e2e -p 'test_model_download_fixture.py'
	python3 -B -m unittest discover -s test/e2e -p 'test_model_files_*.py'
