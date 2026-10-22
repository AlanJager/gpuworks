E2E_LOCALE ?= en
E2E_TOKEN_CASE ?= all
.PHONY: test-e2e-platform-token-options
test-e2e-platform-token-options: build
	@status=0; cases='$(E2E_TOKEN_CASE)'; \
	if [ "$$cases" = all ]; then cases='read-0 read-30 read-90 read-365 write-0 write-30 write-90 write-365'; fi; \
	for case in $$cases; do \
		NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/platform_token_options.py --case "$$case" --locale '$(E2E_LOCALE)' || status=1; \
	done; exit $$status
