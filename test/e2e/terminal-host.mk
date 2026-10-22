E2E_LOCALE ?= en

.PHONY: test-e2e-terminal-host
## Host web terminal through a real local agent; no container or GPU.
test-e2e-terminal-host: test-e2e-terminal-host-fixture build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/terminal_host.py --locale $(E2E_LOCALE)

.PHONY: test-e2e-terminal-host-fixture
test-e2e-terminal-host-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_terminal_host
