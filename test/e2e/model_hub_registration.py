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


TABLE_HELPERS = tuple('test/e2e/' + name for name in (
    'model_metadata_fixture.py', 'model_metadata_table_owner.py',
    'model_metadata_table_oracle.py', 'model_metadata_table_browser.py',
    'model_metadata_table_hook.py', 'metadata_preflight.py',
    'declared-100-repositories.json'))


def family_parser():
    parser = case_parser(__doc__, tuple(CASES))
    parser.add_argument('--check-family', choices=('all', 'metadata-table'), default='all',
        help='Both HF selections run the complete original HF lifecycle and the single ModelsPage table authority')
    return parser


def selected_options(args, parser):
    if args.check_family == 'metadata-table' and args.case != 'huggingface':
        parser.error('--check-family metadata-table requires --case huggingface')
    options = {'check_family': args.check_family}
    if args.case == 'huggingface':
        options['post_exercise'] = after_hf
        options['server_options'] = {'auto_check_updates': False,
                                     'env_overrides': {'GPUWORKS_USAGE_REPORT': 'off'}}
    return options


def after_hf(page, context, server, artifacts, report, locale, data, token, holder):
    # The runner calls this only after the entire original HF exercise returns.
    from model_metadata_table_hook import run_model_table
    run_model_table(context, server, report, locale, data, token, artifacts)


def main():
    parser = family_parser()
    args = parser.parse_args()
    options = selected_options(args, parser)
    contract, exercise = CASES[args.case]
    return run_case(args, contract_id=contract,
        manifest="test/e2e/coverage/model-hub-" + args.case + ".json",
        test_file="test/e2e/model_hub_registration.py", prepare=prepare, exercise=exercise, cleanup=cleanup,
        docker_required=False, **options, helpers=("test/e2e/model_hub_fixture.py", "test/e2e/model_hub_fixture_setup.py",
        "test/e2e/platform_token_read_revoke.py", "test/e2e/deployment_replicas.py",
        "test/e2e/deployment_replica_fixture.py", "test/e2e/deployment_container_fixture.py",
        "test/e2e/model_hub_register_inventory.py", "test/e2e/model_hub_quick_inventory.py")
        + (TABLE_HELPERS if args.case == "huggingface" else ()))


if __name__ == "__main__":
    raise SystemExit(main())
