.PHONY: test-e2e-mcp-copied-config
test-e2e-mcp-copied-config: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/mcp_copied_config.py --case endpoint
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/mcp_copied_config.py --case generic-json
