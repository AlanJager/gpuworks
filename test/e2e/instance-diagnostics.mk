.PHONY: test-e2e-instance-diagnostics-fixture test-e2e-instance-diagnostics

test-e2e-instance-diagnostics-fixture:
	PYTHONPATH=test/e2e python3 -B -m unittest test_instance_diagnostics test_deployment_override_fixture test_deployment_container_fixture test_local_business_runner -v

# build and image preparation are explicit; this target never pulls images.
test-e2e-instance-diagnostics: build test-e2e-instance-diagnostics-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 -B test/e2e/instance_diagnostics.py --locale $(or $(E2E_LOCALE),en)
