LOCALE ?= en
.PHONY: test-e2e-deployment-health test-e2e-deployment-health-fixture
test-e2e-deployment-health: build test-e2e-deployment-health-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 -B test/e2e/deployment_health.py --locale $(LOCALE)
test-e2e-deployment-health-fixture:
	go test ./test/e2e/testdata/deployment-health-proxy
	PYTHONPATH=test/e2e python3 -B -m unittest test_deployment_health_fixture -v
