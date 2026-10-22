.PHONY: test-e2e-image-url-runtime
test-e2e-image-url-runtime: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_runtime.py --case acquire-remove
