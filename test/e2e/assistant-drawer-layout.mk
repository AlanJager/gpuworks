.PHONY: test-e2e-assistant-drawer-layout
test-e2e-assistant-drawer-layout: ui
	mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/gpuworks ./cmd/gpuworks
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/assistant_drawer_layout.py --target-ref $(or $(E2E_TARGET_REF),HEAD) --locale en
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/assistant_drawer_layout.py --target-ref $(or $(E2E_TARGET_REF),HEAD) --locale zh
