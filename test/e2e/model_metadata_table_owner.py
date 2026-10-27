"""Unused real-API owner for a strict metadata-only table preflight.

request(method, path, body=None) is authenticated transport, never a management
response stub in production use. Tests inject a rejecting stateful protocol.
"""
import copy
import math
from urllib.parse import urlencode

MODELS = '/api/v1/models'
RULES = '/api/v1/pull/source-rules'
RULE_FIELDS = ('match', 'endpoints', 'allow_upstream', 'control_plane', 'note', 'node_requirements')


def require(value, message):
    if not value:
        raise AssertionError(message)


def identity(document, row):
    return all(document.get(k) == v for k, v in
               {'source': row.source, 'source_id': row.repo, 'revision': row.revision}.items())


class MetadataTableOwner:
    def __init__(self, request, hub, rule_ids, report):
        require(len(hub.rows) == 100 and len({(r.source, r.repo, r.revision) for r in hub.rows}) == 100,
                'owner requires exactly100 distinct declared artifacts')
        require(set(rule_ids) == {'huggingface', 'modelscope'}, 'two owned source-rule IDs required')
        require(len(set(rule_ids.values())) == 2 and all(type(x) is int and x > 0 for x in rule_ids.values()),
                'source-rule IDs must be distinct positive integers')
        self.request, self.hub, self.rule_ids, self.report = request, hub, rule_ids, report
        self.owned, self.attempted, self.confirmed, self.restore = {}, {}, set(), []
        self.namespace = hub.rows[0].repo.split('/')[0]
        report.update(status='NOT_RUN', attempted=[], owned_ids=[], checkpoints=[], cleanup=[],
                      cleanup_errors=[], remaining_ids=[], unresolved_attempts=[], primary_error=None,
                      api_calls=[], registrations=[])

    def call(self, method, path, body=None):
        event = {'method': method, 'path': path, 'body': copy.deepcopy(body)}
        self.report['api_calls'].append(event)
        try:
            status, result = self.request(method, path, body)
        except Exception as error:
            event['error'] = type(error).__name__ + ': ' + str(error)
            raise
        event['status'] = status
        # Metadata-only DTOs contain no credentials; capture failures before assertions.
        if path.startswith(MODELS):
            event['response'] = copy.deepcopy(result)
        require(type(status) is int, 'HTTP status must be an integer')
        return status, result

    def rules(self):
        status, result = self.call('GET', RULES)
        require(status == 200 and isinstance(result, dict) and isinstance(result.get('items'), list),
                'source-rule list failed')
        return {r['id']: r for r in result['items']}

    def rule_body(self, document):
        require(all(k in document for k in RULE_FIELDS), 'source-rule DTO missing fields')
        return {k: copy.deepcopy(document[k]) for k in RULE_FIELDS}

    def configure(self):
        self.hub.start()
        existing = self.rules()
        for source, ident in self.rule_ids.items():
            require(ident in existing, 'owned source-rule missing')
            original = self.rule_body(existing[ident])
            canonical = 'https://huggingface.co' if source == 'huggingface' else 'https://modelscope.cn'
            require(original['match'] == canonical and original['allow_upstream'] is False
                    and original['control_plane'] is True, 'source rule escapes owned metadata boundary')
            replacement = {**original, 'endpoints': [self.hub.base + ('/hf' if source == 'huggingface' else '/ms')]}
            # Record before PUT: an error/timeout may occur after the real update committed.
            self.restore.append((ident, original))
            status, result = self.call('PUT', RULES + '/' + str(ident), replacement)
            require(status == 200 and self.rule_body(result) == replacement, 'source-rule update mismatch')
            require(self.rule_body(self.rules()[ident]) == replacement, 'source-rule readback mismatch')
            status, result = self.call('GET', '/api/v1/pull/metadata-sources?' + urlencode({'source': source}))
            require(status == 200 and result == {'source': source, 'bases': replacement['endpoints']},
                    'metadata source selection mismatch')

    def listing(self, query, source=None):
        result, page, total = [], 1, None
        while True:
            args = {'q': query, 'page': page, 'page_size': 100, 'sort': 'name'}
            if source:
                args['source'] = source
            status, document = self.call('GET', MODELS + '?' + urlencode(args))
            require(status == 200 and isinstance(document, dict), 'paged model lookup failed')
            items = document.get('items')
            require(isinstance(items, list) and type(document.get('total')) is int
                    and document['total'] >= 0 and document.get('page') == page
                    and document.get('page_size') == 100, 'invalid model page DTO')
            if total is None:
                total = document['total']
            require(total == document['total'], 'model total changed during scoped lookup')
            require(len(items) == min(100, max(total - (page - 1) * 100, 0)), 'model page is incomplete')
            result.extend(items)
            if page >= max(1, math.ceil(total / 100)):
                break
            page += 1
        ids = [x.get('id') for x in result]
        require(all(type(x) is int and x > 0 for x in ids) and len(set(ids)) == len(ids),
                'paged model lookup repeats or omits identity')
        return result

    def bind(self, ident, row):
        require(type(ident) is int and ident > 0, 'model response lacks usable ID')
        require(ident not in self.owned or self.owned[ident] == row, 'different repositories reused model ID')
        self.owned[ident] = row
        self.report['owned_ids'] = sorted(self.owned)

    def facts(self, document, row):
        require(identity(document, row), 'model artifact identity differs from declaration')
        for key, expected in row.expected_public_facts().items():
            require(document.get(key) == expected, 'model static fact mismatch: ' + key)
        require(not document.get('metadata_notice') and not document.get('nodes') and not document.get('storages'),
                'metadata registration has notice or materialized files')

    def discover(self, row):
        # q is a supported substring search; source is a supported exact filter.
        # Never confuse search membership with the exact ownership predicate.
        for document in self.listing(row.repo, row.source):
            if identity(document, row):
                self.bind(document['id'], row)

    def checkpoint(self, count, callback):
        declared = self.hub.rows[:count]
        expected = {row.repo: row for row in declared}
        listed = self.listing(self.namespace)
        require(len(listed) == count, 'checkpoint count differs from declared corpus')
        require({m.get('source_id') for m in listed} == set(expected), 'checkpoint repository set differs')
        require({m['id'] for m in listed} == set(self.owned), 'checkpoint ID set differs')
        for document in listed:
            row = expected[document['source_id']]
            require(identity(document, row) and document.get('name') == row.name,
                    'checkpoint list identity or name differs')
        self.report['checkpoints'].append({'count': count, 'ids': sorted(self.owned),
                                           'repos': sorted(expected)})
        if callback:
            callback(count, declared, dict(self.owned))

    def run(self, checkpoint=None):
        try:
            self.configure()
            require(self.listing(self.namespace) == [], 'owned namespace is not pristine')
            for count, row in enumerate(self.hub.rows, 1):
                self.attempted[row.repo] = row
                self.report['attempted'].append(row.repo)
                status, response = self.call('POST', MODELS, row.registration_body())
                if isinstance(response, dict) and type(response.get('id')) is int:
                    self.bind(response['id'], row)  # Capture even a failing response before assertion.
                require(status == 201 and isinstance(response, dict), 'model registration failed')
                require(type(response.get('id')) is int and response['id'] > 0,
                        'model response lacks usable ID')
                status, saved = self.call('GET', MODELS + '/' + str(response['id']))
                require(status == 200 and saved.get('id') == response['id'], 'independent model read failed')
                self.facts(response, row)
                self.facts(saved, row)
                self.report['registrations'].append({'id': saved['id'], 'repo': row.repo,
                    'expected': row.expected_public_facts(),
                    'saved': {k: saved.get(k) for k in row.expected_public_facts()}})
                self.confirmed.add(row.repo)
                if count in (10, 30, 100):
                    self.checkpoint(count, checkpoint)
        except Exception as error:
            self.report['primary_error'] = type(error).__name__ + ': ' + str(error)
        finally:
            self.cleanup()
        self.report['status'] = ('PASS' if not self.report['primary_error'] and not self.report['cleanup_errors']
                                 and not self.report['remaining_ids'] and not self.report['unresolved_attempts']
                                 else 'FAIL')
        return self.report

    def cleanup(self):
        # Discover attempted-but-unconfirmed commits, including timeouts after commit.
        for repo, row in self.attempted.items():
            if repo not in self.confirmed:
                try:
                    self.discover(row)
                except Exception as error:
                    self.report['unresolved_attempts'].append(repo)
                    self.report['cleanup_errors'].append('discover ' + repo + ': ' + str(error))
        gone = set()
        for ident, row in list(self.owned.items()):
            try:
                path = MODELS + '/' + str(ident)
                status, document = self.call('GET', path)
                if status != 404:
                    require(status == 200 and identity(document, row), 'cleanup identity guard refused deletion')
                    status, _ = self.call('DELETE', path)
                    require(status == 204, 'model cleanup DELETE failed')
                status, _ = self.call('GET', path)
                require(status == 404, 'model cleanup GET did not prove404')
                gone.add(ident)
                self.report['cleanup'].append({'id': ident, 'get': 404})
            except Exception as error:
                self.report['cleanup_errors'].append('model ' + str(ident) + ': ' + str(error))
        self.report['remaining_ids'] = sorted(set(self.owned) - gone)
        for row in self.attempted.values():
            try:
                require(not any(identity(m, row) for m in self.listing(row.repo, row.source)),
                        'owned repository remains after cleanup')
            except Exception as error:
                self.report['cleanup_errors'].append('absence ' + row.repo + ': ' + str(error))
        for ident, original in reversed(self.restore):
            try:
                status, document = self.call('PUT', RULES + '/' + str(ident), original)
                require(status == 200 and self.rule_body(document) == original, 'source-rule restore failed')
                require(self.rule_body(self.rules()[ident]) == original, 'source-rule restore readback differs')
            except Exception as error:
                self.report['cleanup_errors'].append('rule ' + str(ident) + ': ' + str(error))
        try:
            stopped = self.hub.stop()
            self.report['hub_stop'] = stopped
            require(stopped == {'listener_stopped': True, 'thread_stopped': True, 'handlers': 0, 'stopped': True},
                    'metadata hub did not release listener/handlers')
        except Exception as error:
            self.report['cleanup_errors'].append('hub stop: ' + str(error))
