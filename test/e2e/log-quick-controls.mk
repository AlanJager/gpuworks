.PHONY: test-e2e-log-quick-controls
test-e2e-log-quick-controls: ensure-bins
	@status=0; for locale in en zh; do \
		for chip in Traceback ERROR WARNING ERR99999 'out of memory' 'no such container'; do \
			NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/log_quick_controls.py --locale "$$locale" --chip "$$chip" || status=1; \
		done; \
	done; exit $$status
