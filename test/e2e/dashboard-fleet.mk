E2E_LOCALE ?= en

.PHONY: test-e2e-dashboard-fleet test-e2e-dashboard-fleet-fixture
test-e2e-dashboard-fleet-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_dashboard_fleet -v

## Dashboard inventory and label grouping: real browser/server/SQLite, no GPU.
test-e2e-dashboard-fleet: build test-e2e-dashboard-fleet-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/dashboard_fleet.py --locale $(E2E_LOCALE)
