.PHONY: test-e2e-deployment-lifecycle
test-e2e-deployment-lifecycle: fake-engine-image build test-e2e-deployment-lifecycle-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/deployment_lifecycle.py --locale $(or $(E2E_LOCALE),en)

.PHONY: test-e2e-deployment-lifecycle-fixture
test-e2e-deployment-lifecycle-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_deployment_lifecycle test_deployment_container_fixture -v
