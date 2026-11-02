.PHONY: test-e2e-image-enable-cycle
test-e2e-image-enable-cycle: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_enable_cycle.py --case enabled-cycle
