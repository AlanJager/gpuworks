E2E_LOCALE ?= en

.PHONY: test-e2e-nodes-labels-search test-e2e-nodes-labels-search-fixture
test-e2e-nodes-labels-search-fixture:
	PYTHONPATH=test/e2e python3 -m unittest test_nodes_labels_search -v

## Offline node definitions: real browser/server/SQLite, fixtures created by API.
test-e2e-nodes-labels-search: build test-e2e-nodes-labels-search-fixture
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/nodes_labels_search.py --locale $(E2E_LOCALE)
