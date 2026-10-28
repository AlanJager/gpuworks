.PHONY: test-e2e-image-failure-retry test-e2e-image-cancel-retry

test-e2e-image-failure-retry: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_transfer_recovery.py --case failure-retry

test-e2e-image-cancel-retry: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_transfer_recovery.py --case cancel-retry
