# Four independent public-provider/automatic-placement contracts; full lifecycle.
# Run: make test-e2e-deployment-acquisition CASE=vllm-huggingface LOCALE=en
.PHONY: test-e2e-deployment-acquisition

test-e2e-deployment-acquisition:
	python3 -B -m unittest discover -s test/e2e -p 'test_deployment_acquisition_*.py'
	python3 -B test/e2e/deployment_acquisition.py --case $(CASE) --locale $(or $(LOCALE),en)
