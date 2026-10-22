E2E_LOCALE ?= en

.PHONY: test-e2e-storage-local
## Local storage form and lifecycle: real browser/server/SQLite, no agent or GPU.
test-e2e-storage-local: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/storage_local.py --locale $(E2E_LOCALE)
