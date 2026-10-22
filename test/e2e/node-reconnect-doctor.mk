E2E_LOCALE ?= en

.PHONY: test-e2e-node-reconnect-doctor
## Node offline/reconnect and host doctor through a real local agent; no GPU.
test-e2e-node-reconnect-doctor: test-e2e-node-reconnect-doctor-fixture build
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/node_reconnect_doctor.py --locale $(E2E_LOCALE)

.PHONY: test-e2e-node-reconnect-doctor-fixture
test-e2e-node-reconnect-doctor-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_node_reconnect_doctor
