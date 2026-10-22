.PHONY: test-e2e-playground-chat-gateway-fixture test-e2e-playground-chat-gateway

## Docker inspect classification: only an explicit missing container is absent;
## daemon and permission errors must fail instead of satisfying cleanup.
test-e2e-playground-chat-gateway-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_playground_chat_gateway -v

test-e2e-playground-chat-gateway: fake-engine-image build test-e2e-playground-chat-gateway-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/playground_chat_gateway.py --locale $(or $(E2E_LOCALE),en)
