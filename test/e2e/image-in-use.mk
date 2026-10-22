.PHONY: test-e2e-image-in-use-remove
test-e2e-image-in-use-remove: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_in_use.py --case in-use-remove
