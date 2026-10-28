.PHONY: test-e2e-image-enums
test-e2e-image-enums: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_catalog_enums.py --case engines
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_catalog_enums.py --case frameworks
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_catalog_enums.py --case architectures
