.PHONY: test-e2e-provider-endpoint-defaults
test-e2e-provider-endpoint-defaults: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/provider_endpoint_defaults.py
