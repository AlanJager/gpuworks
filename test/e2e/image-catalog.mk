.PHONY: test-e2e-image-register test-e2e-image-version-delete
## Metadata-only image contracts; each case owns a fresh server and SQLite database.
test-e2e-image-register: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_catalog.py --case register

test-e2e-image-version-delete: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_catalog.py --case delete-version
