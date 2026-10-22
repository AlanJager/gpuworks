E2E_LOCALE ?= en

.PHONY: test-e2e-assistant-fixture test-e2e-assistant-service-tools
## The LLM endpoint is an auditable fixture; server, tools, SQLite and browser
## stay real. No GPU or container is required.
test-e2e-assistant-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_assistant_fixture test_assistant_service_tools -v

test-e2e-assistant-service-tools: build test-e2e-assistant-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/assistant_service_tools.py --locale $(E2E_LOCALE)
