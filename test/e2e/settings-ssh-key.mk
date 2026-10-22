E2E_LOCALE ?= en

.PHONY: test-e2e-settings-ssh-key test-e2e-settings-ssh-key-fixture
test-e2e-settings-ssh-key-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_artifact_redaction test_settings_ssh_key -v

## Platform SSH key rotation/import: real browser/server/SQLite, no GPU or sshd.
test-e2e-settings-ssh-key: build test-e2e-settings-ssh-key-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/settings_ssh_key.py --locale $(E2E_LOCALE)
