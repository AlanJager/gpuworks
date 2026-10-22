E2E_LOCALE ?= en

.PHONY: test-e2e-settings-deployment-policy
## Cluster-wide deployment start timeout: real browser/server/SQLite, no GPU.
test-e2e-settings-deployment-policy: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/settings_deployment_policy.py --locale $(E2E_LOCALE)
