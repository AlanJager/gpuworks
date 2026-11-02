.PHONY: test-e2e-provider-endpoint-edit

# Real browser + isolated server and strict local RunPod boundary.
test-e2e-provider-endpoint-edit: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/provider_endpoint_edit.py --locale en
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/provider_endpoint_edit.py --locale zh
