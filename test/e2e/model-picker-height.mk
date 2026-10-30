.PHONY: test-e2e-model-picker-height-unit test-e2e-model-picker-height

test-e2e-model-picker-height-unit:
	PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=test/e2e python3 -m unittest model_picker_height.test_case test_local_business_runner test_model_hub_setup -v

# Real server/SQLite and both locales; no GPU, agent or remote model download.
test-e2e-model-picker-height: test-e2e-model-picker-height-unit
	$(MAKE) server-linux-amd64
	PYTHONPATH=test/e2e python3 -m model_picker_height.case --server-bin bin/gpuworks-linux-amd64 --locale en --target-ref $(or $(E2E_TARGET_REF),wei/master) --browser-channel $(or $(E2E_BROWSER_CHANNEL),chrome)
	PYTHONPATH=test/e2e python3 -m model_picker_height.case --server-bin bin/gpuworks-linux-amd64 --locale zh --target-ref $(or $(E2E_TARGET_REF),wei/master) --browser-channel $(or $(E2E_BROWSER_CHANNEL),chrome)
