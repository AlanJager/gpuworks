.PHONY: test-e2e-deployment-container-overrides-fixture test-e2e-deployment-container-overrides

test-e2e-deployment-container-overrides-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_deployment_override_fixture test_deployment_override_runner test_deployment_wizard_fixture test_deployment_wizard_runner test_deployment_container_fixture test_local_browser_fixture -v

# Local images must already exist; derived image build uses no pulls/network.
test-e2e-deployment-container-overrides: build test-e2e-deployment-container-overrides-fixture
	PYTHONPATH=test/e2e python3 -c 'import deployment_override_create as case; case.require_dedicated_docker()'
	$(MAKE) fake-engine-image
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/deployment_override_create.py --locale $(or $(E2E_LOCALE),en)
