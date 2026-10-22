.PHONY: test-e2e-deployment-sleep-wake-fixture test-e2e-deployment-sleep-wake

## Docker inspect classification: only an explicit missing container/object is
## absent; DNS, daemon and permission errors must fail instead of satisfying cleanup.
test-e2e-deployment-sleep-wake-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_deployment_sleep_wake test_deployment_container_fixture -v

test-e2e-deployment-sleep-wake: fake-engine-image build test-e2e-deployment-sleep-wake-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/deployment_sleep_wake.py --locale $(or $(E2E_LOCALE),en)
