.PHONY: test-e2e-model-hub-fixture test-e2e-model-hub-registration

test-e2e-model-hub-fixture:
	PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=test/e2e python3 -m unittest test_model_hub_fixture test_model_hub_setup -v

# Three independent Chrome/server/SQLite registrations; metadata origins are local HTTP fixtures.
test-e2e-model-hub-registration: build test-e2e-model-hub-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/model_hub_registration.py --case huggingface --locale $(or $(E2E_LOCALE),en)
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/model_hub_registration.py --case modelscope --locale $(or $(E2E_LOCALE),en)
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/model_hub_registration.py --case quick --locale $(or $(E2E_LOCALE),en)
