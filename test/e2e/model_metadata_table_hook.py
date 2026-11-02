"""UNAPPLIED single-authority hook, invoked only after original HF finish()."""
import copy
import hashlib
import json
from local_browser_fixture import http, ROOT
from model_metadata_fixture import MetadataTableHub
from model_metadata_table_owner import MetadataTableOwner, MODELS, RULES, require
from metadata_preflight import check_external


def frozen(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def borrowed_rules(request, original):
    status, listing = request('GET', RULES)
    require(status == 200, 'owned rule discovery failed')
    ids = set(original['owned_source_rule_ids'])
    require(len(ids) == 2, 'original owner must hold exactly two rules')
    result = {}
    for rule in listing['items']:
        if rule['id'] not in ids:
            continue
        source = {'https://huggingface.co': 'huggingface', 'https://modelscope.cn': 'modelscope'}.get(rule['match'])
        require(source and source not in result and rule['note'] == original['owned_source_rule_note']
                and rule['endpoints'] == [original['fixture_origins'][source]]
                and rule['allow_upstream'] is False and rule['control_plane'] is True,
                'borrowed rule exact ownership/readback differs')
        result[source] = rule['id']
    require(set(result) == {'huggingface', 'modelscope'}, 'borrowed rule set incomplete')
    return result


TABLE_INPUTS = tuple('test/e2e/' + name for name in (
    'model_metadata_fixture.py', 'model_metadata_table_owner.py',
    'model_metadata_table_oracle.py', 'model_metadata_table_browser.py',
    'model_metadata_table_hook.py', 'metadata_preflight.py',
    'declared-100-repositories.json'))


def verify_bound_sources(report, source_root=ROOT):
    # These MUST already exist in source_identity's pre-server-start receipt.
    # This function only verifies; it never writes/fills missing source hashes.
    require(report.get('check_family') in ('all', 'metadata-table'), 'table family was not selected at entry')
    require(report.get('contract') == 'gpuworks/models/huggingface-browser-registration', 'table authority is not HF')
    bound = report.get('source_hashes', {})
    for relative in TABLE_INPUTS:
        require(relative in bound, 'table input was not frozen before server start: ' + relative)
        require(hashlib.sha256((source_root / relative).read_bytes()).hexdigest() == bound[relative],
                'prebound table input changed: ' + relative)
    corpus = json.loads((source_root / 'test/e2e/declared-100-repositories.json').read_text())
    from model_metadata_fixture import declared_rows, CONFIG, INDEX, README, SHA, WEIGHT_BYTES
    require(corpus['metadata'] == {'config': json.loads(CONFIG), 'index': json.loads(INDEX),
            'readme': README, 'sha': SHA, 'weight_bytes': WEIGHT_BYTES}, 'static corpus metadata differs')
    declared = declared_rows(corpus['namespace'])
    require(corpus['rows'] == [{'ordinal': row.number, 'registration': row.registration_body(),
            'expected_public_facts': row.expected_public_facts()} for row in declared], 'static corpus rows differ')


def capture_checkpoint_failure(checkpoint, page, artifacts, result, sensitive=False):
    """Capture the actual Table page synchronously before owner finally cleans it."""
    def checked_checkpoint(count, rows, owned):
        try:
            return checkpoint(count, rows, owned)
        except Exception:
            capture = result['failure_capture'] = {'checkpoint_count': count, 'capture_error': []}
            # Use the existing sensitive-phase guard, then preserve this screenshot
            # separately before the outer runner captures its original parent page.
            try:
                captured = artifacts.capture_failure(page, sensitive)
                if captured == 'failure.png':
                    name = 'model-table-failure.png'
                    (artifacts.directory / captured).replace(artifacts.directory / name)
                    capture['screenshot'] = name
                else:
                    capture['screenshot'] = captured
            except Exception as error:
                capture['capture_error'].append(artifacts.redact('screenshot: ' + type(error).__name__ + ': ' + str(error)))
            try:
                name = 'model-table-failure.aria.txt'
                snapshot = page.locator('body').aria_snapshot()
                (artifacts.directory / name).write_text(artifacts.redact(snapshot))
                capture['aria'] = name
            except Exception as error:
                capture['capture_error'].append(artifacts.redact('aria: ' + type(error).__name__ + ': ' + str(error)))
            raise
    return checked_checkpoint


def run_model_table(context, server, report, locale, data, token, artifacts):
    verify_bound_sources(report)
    from model_metadata_table_browser import ModelsTable
    original_keys = ('requests', 'browser_failures', 'page_errors', 'console_errors', 'evidence', 'observations')
    snapshot = frozen({k: copy.deepcopy(report.get(k)) for k in original_keys})
    audit = copy.deepcopy(data['hub'].audit())
    result = report['model_table'] = {'status': 'NOT_RUN', 'browser_executed': False, 'coverage_credit': 0,
                                    'primary_error': None, 'cleanup_errors': []}
    page, owner, hub, rules_before = None, None, None, None
    request = lambda method, path, body=None: http(server.base, method, path, token, body)
    try:
        for ident in report.get('owned_model_ids', []):
            require(request('GET', MODELS + '/' + str(ident))[0] == 404, 'original HF lifecycle incomplete')
        ids = borrowed_rules(request, report)
        rules_before = copy.deepcopy(request('GET', RULES)[1])
        hub = MetadataTableHub(data['repo'].split('/')[0] + '-table')
        page = context.new_page()
        require(page.viewport_size == {'width': 1440, 'height': 900}, 'context formal viewport differs')
        owner = MetadataTableOwner(request, hub, ids, result.setdefault('owner', {}))
        adapter = ModelsTable(page, server, owner, locale, result.setdefault('browser', {}))
        result['browser_executed'] = True
        owned_result = owner.run(checkpoint=capture_checkpoint_failure(
            adapter.checkpoint, page, artifacts, result, bool(report.get('sensitive_phase'))))
        require(owned_result['status'] == 'PASS', owned_result.get('primary_error') or str(owned_result['cleanup_errors']))
        adapter.verify_errors()
        verify_bound_sources(report)
        result['external'] = check_external(hub.rows, hub.audit())
        require(snapshot == frozen({k: report.get(k) for k in original_keys}), 'original HF evidence changed')
        require(data['hub'].audit() == audit, 'original HF exact source audit changed')
        result['status'] = 'PASS'
    except Exception as error:
        result['primary_error'] = type(error).__name__ + ': ' + str(error)
        result['status'] = 'FAIL'
    finally:
        # Preserve first primary error even if independent closure fails.
        for label, close in [('page', lambda: page.close() if page else None),
                             ('hub', lambda: hub.stop() if hub else None)]:
            try:
                close()
            except Exception as error:
                result['cleanup_errors'].append(label + ': ' + str(error))
        try:
            if rules_before is not None:
                status, rules_after = request('GET', RULES)
                require(status == 200 and rules_after == rules_before, 'source-rule list not exactly restored')
            require(snapshot == frozen({k: report.get(k) for k in original_keys}), 'original HF evidence changed')
            require(data['hub'].audit() == audit, 'original HF source audit changed')
        except Exception as error:
            result['cleanup_errors'].append(str(error))
        if owner:
            result['cleanup_errors'].extend(owner.report.get('cleanup_errors', []))
        if result['cleanup_errors']:
            result['status'] = 'FAIL'
    require(result['status'] == 'PASS', result['primary_error'] or str(result['cleanup_errors']))
