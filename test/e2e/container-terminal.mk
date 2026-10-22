.PHONY: test-e2e-container-terminal-fixture test-e2e-container-terminal

test-e2e-container-terminal-fixture:
	PYTHONPATH=test/e2e python3 -B -m unittest test_container_terminal test_instance_diagnostics test_local_business_runner -v

test-e2e-container-terminal: build test-e2e-container-terminal-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 -B test/e2e/container_terminal.py --locale $(or $(E2E_LOCALE),en)
