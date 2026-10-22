.PHONY: test-e2e-image-filters
test-e2e-image-filters: build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_catalog_filters.py --case filters
