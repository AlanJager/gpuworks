#!/usr/bin/env python3
"""Real Chrome Hub metadata registration against real server/SQLite/source rules.

E2E standard: test/e2e/browser-e2e-testing-standard.md.
Run: make test-e2e-model-hub-registration E2E_LOCALE=en (or zh).
After build: python3 test/e2e/model_hub_registration.py --case huggingface --locale en
Cases: huggingface (browser metadata, commit pinning, backend name refusal, duplicate);
modelscope (main->master,503 best-effort registration/refresh recovery); quick
(template-selected source registration, async metadata and reuse).
No browser management mocks, remote downloads, agent, container or GPU claims.
Failed owned state stays private under ignored secrets/e2e/<runId> for seven days;
only this invocation's models, template, rules and listeners are reclaimed.
"""
from local_business_runner import case_parser, run_case
from model_hub_fixture_setup import prepare, cleanup
from model_hub_registration_browser import test_huggingface, test_modelscope, test_quick

CASES = {"huggingface": ("gpuworks/models/huggingface-browser-registration", test_huggingface),
         "modelscope": ("gpuworks/models/modelscope-registration-recovery", test_modelscope),
         "quick": ("gpuworks/models/template-quick-registration", test_quick)}


def main():
    args = case_parser(__doc__, tuple(CASES)).parse_args()
    contract, exercise = CASES[args.case]
    return run_case(args, contract_id=contract,
        manifest="test/e2e/coverage/model-hub-" + args.case + ".json",
        test_file="test/e2e/model_hub_registration.py", prepare=prepare, exercise=exercise, cleanup=cleanup,
        docker_required=False, helpers=("test/e2e/model_hub_fixture.py", "test/e2e/model_hub_fixture_setup.py",
        "test/e2e/platform_token_read_revoke.py", "test/e2e/deployment_replicas.py",
        "test/e2e/deployment_replica_fixture.py", "test/e2e/deployment_container_fixture.py",
        "test/e2e/model_hub_register_inventory.py", "test/e2e/model_hub_quick_inventory.py"))


if __name__ == "__main__":
    raise SystemExit(main())
