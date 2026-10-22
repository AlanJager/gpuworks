.PHONY: test-e2e-storage-shared-dir test-e2e-storage-shared-dir-fixture
test-e2e-storage-shared-dir-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_storage_shared_fixture -v

test-e2e-storage-shared-dir: build test-e2e-storage-shared-dir-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/storage_shared_dir.py --locale $(or $(E2E_LOCALE),en)
