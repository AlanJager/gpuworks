E2E_LOCALE ?= en

.PHONY: test-e2e-models-local-register
## Local model registration through a real local agent; no container or GPU.
test-e2e-models-local-register: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/models_local_register.py --locale $(E2E_LOCALE)
