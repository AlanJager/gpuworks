.PHONY: test-e2e-deployment-wizard-fixture test-e2e-deployment-wizard-create

test-e2e-deployment-wizard-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_deployment_wizard_fixture test_deployment_wizard_runner test_deployment_container_fixture test_local_browser_fixture -v

# Real Chrome -> server/SQLite -> agent -> Docker fake-engine -> gateway.
test-e2e-deployment-wizard-create: fake-engine-image build test-e2e-deployment-wizard-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/deployment_wizard_create.py --locale $(or $(E2E_LOCALE),en)
