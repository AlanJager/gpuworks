.PHONY: test-e2e-http-model-registration
## HTTP model registration and source-loss recovery: isolated server + audited directory.
test-e2e-http-model-registration: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/http_model_registration.py
