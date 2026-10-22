.PHONY: test-e2e-image-registry-pull test-e2e-image-registry-mirror test-e2e-image-registry-fixture

test-e2e-image-registry-fixture:
	python3 -m unittest discover -s test/e2e -p test_image_registry_fixture.py -v

test-e2e-image-registry-pull: build test-e2e-image-registry-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_registry.py --case registry-pull

test-e2e-image-registry-mirror: build test-e2e-image-registry-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_registry.py --case registry-mirror
