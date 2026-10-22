.PHONY: test-e2e-storage-nfs-cache-policy test-e2e-storage-nfs-cache-create
## NFS cache node policy: real browser/server/SQLite with seeded offline nodes; no agent or mount.
test-e2e-storage-nfs-cache-policy: build test-e2e-storage-nfs-cache-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/storage_cache_policy.py --locale $(or $(E2E_LOCALE),en)

## NFS cache four-step creation: real browser/server/agent and an owned NFS export (E2E_NFS_EXPORT=host:/path).
test-e2e-storage-nfs-cache-create: build test-e2e-storage-nfs-cache-fixture
	$(if $(E2E_NFS_EXPORT),,$(error E2E_NFS_EXPORT=host:/path of an owned NFS export is required))
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/storage_cache_create.py --export '$(E2E_NFS_EXPORT)' --locale $(or $(E2E_LOCALE),en)

.PHONY: test-e2e-storage-nfs-cache-fixture
test-e2e-storage-nfs-cache-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_storage_cache_cleanup -v
