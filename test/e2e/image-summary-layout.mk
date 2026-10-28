.PHONY: test-e2e-image-summary-layout
test-e2e-image-summary-layout: ui
	mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/gpuworks ./cmd/gpuworks
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_summary_layout.py --case summary-layout --locale en
	NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost python3 test/e2e/image_summary_layout.py --case summary-layout --locale zh
