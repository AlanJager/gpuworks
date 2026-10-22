REPLICA_CASE ?= scale
LOCALE ?= en
.PHONY: test-e2e-deployment-replicas test-e2e-deployment-replicas-fixture
test-e2e-deployment-replicas: build test-e2e-deployment-replicas-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/deployment_replicas.py --case $(REPLICA_CASE) --locale $(LOCALE)
test-e2e-deployment-replicas-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_deployment_replica_fixture test_deployment_replica_runner test_deployment_container_fixture -v
	python3 -m py_compile test/e2e/deployment_replicas.py test/e2e/deployment_replica_browser.py test/e2e/deployment_replica_fixture.py
