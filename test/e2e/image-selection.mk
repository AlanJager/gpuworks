.PHONY: test-e2e-image-community-prefill test-e2e-image-archive-prefill

test-e2e-image-community-prefill: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_selection.py --case community-prefill

test-e2e-image-archive-prefill: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_selection.py --case archive-prefill
