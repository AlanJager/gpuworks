.PHONY: test-e2e-session-logout
test-e2e-session-logout: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/session_logout.py
