E2E_LOCALE ?= en

.PHONY: test-e2e-settings-branding
## Branding text/logo: real browser, server and public sign-in page; no GPU.
test-e2e-settings-branding: build test-e2e-settings-branding-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/settings_branding.py --locale $(E2E_LOCALE)

.PHONY: test-e2e-settings-branding-fixture
test-e2e-settings-branding-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_settings_branding_fixture -v
