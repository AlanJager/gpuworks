E2E_LOCALE ?= en

.PHONY: test-e2e-settings-telemetry test-e2e-settings-telemetry-fixture
## Request trace export settings with a local OTLP/HTTP collector fixture.
test-e2e-settings-telemetry: ensure-bins
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/settings_telemetry.py --locale $(E2E_LOCALE)

## Behavior tests for the OTLP collector fixture.
test-e2e-settings-telemetry-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_otlp_fixture -v
