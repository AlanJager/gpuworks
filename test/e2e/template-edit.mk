.PHONY: test-e2e-template-edit
## Three independent template configuration cases; real CodeMirror/server/SQLite, no GPU.
test-e2e-template-edit: ensure-bins
	PYTHONPATH=test/e2e python3 -m unittest test_template_catalog_fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/template_edit.py --locale $(or $(E2E_LOCALE),en)
