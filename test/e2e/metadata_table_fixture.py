"""Declared image metadata corpora, real import/GET/delete and scoped ownership."""
import copy
import json
from platform_token_read_revoke import check, collection

SIZES = (10, 30, 100)
PAGE_SIZES = (10, 20, 50, 100)


def same(actual, expected, label):
    dump = lambda x: json.dumps(x, sort_keys=True, allow_nan=False)
    check(dump(actual) == dump(expected), label + ': expected=' + repr(expected) + '; actual=' + repr(actual))


def corpus(prefix, count, engine, framework):
    check(count in (*SIZES, 21), 'undeclared metadata corpus size')
    return [{'name': f'{prefix}-{i:03}', 'engine': engine, 'framework': framework,
             'source': 'custom', 'enabled': i % 2 == 0, 'versions': [{
                 'image_ref': f'registry.example/{prefix}/runner:{i:03}',
                 'engine_version': '1.0', 'framework_version': '1.0', 'arch': 'multi',
                 'source': 'custom', 'rolling_tag': False, 'registry_sources': []}]}
            for i in range(count)]


def page_names(names, page, size):
    check(size in PAGE_SIZES and type(page) is int and page > 0, 'invalid declared page')
    return sorted(names)[(page - 1) * size:page * size]


def assert_traversal(pages, names, size, backwards=False):
    count = (len(names) + size - 1) // size
    indexes = list(range(1, count + 1))
    if backwards:
        indexes.reverse()
    same(len(pages), count, 'metadata traversal page count')
    same(pages, [page_names(names, i, size) for i in indexes], 'complete independent page slices')
    flattened = [name for page in pages for name in page]
    same(sorted(flattened), sorted(names), 'metadata traversal no omission/duplicate')


class OwnedCorpus:
    def __init__(self, page, base, baseline, report):
        self.page, self.base, self.baseline = page, base, copy.deepcopy(baseline)
        self.state = report['metadata_table']
        self.owned = self.state['remaining_resources']

    def listed(self):
        response = self.page.request.get(self.base + '/api/v1/images')
        check(response.status == 200, 'metadata corpus collection GET failed')
        return collection(response)

    def register(self, rows):
        names = {row['name'] for row in rows}
        check(len(names) == len(rows), 'duplicate declared corpus name')
        self.state['pending_declared_names'] = sorted(names)
        response = self.page.request.post(self.base + '/api/v1/images/import', data={'series': rows})
        # Discover and record exact declared names before checking success or
        # counts: a partial import must retain cleanup ownership on failure.
        listed = self.listed()
        found = [row for row in listed if row['name'] in names]
        for row in found:
            check(not any(old['id'] == row['id'] for old in self.owned), 'corpus ownership collision')
            self.owned.append({'kind': 'image-series', 'id': row['id'], 'name': row['name']})
        self.state.setdefault('imports', []).append({'method': 'POST', 'path': '/api/v1/images/import',
            'query': '', 'body': {'series': copy.deepcopy(rows)}, 'status': response.status})
        check(response.status == 200, 'metadata corpus import failed')
        same(sorted(row['name'] for row in found), sorted(names), 'real declared corpus complete')
        expected = {row['name']: row for row in rows}
        for row in found:
            declaration = expected[row['name']]
            same({k: row[k] for k in ('name', 'engine', 'framework', 'source', 'enabled')},
                 {k: declaration[k] for k in ('name', 'engine', 'framework', 'source', 'enabled')},
                 'independent corpus list facts')
            same({k: row[k] for k in ('version_count', 'present_nodes', 'total_nodes')},
                 {'version_count': 1, 'present_nodes': 0, 'total_nodes': 0}, 'metadata only corpus counts')
            detail = self.page.request.get(self.base + '/api/v1/images/' + str(row['id']))
            check(detail.status == 200, 'corpus detail GET failed')
            versions = detail.json()['versions']
            same(len(versions), 1, 'one declared metadata version')
            same({k: versions[0][k] for k in ('image_ref', 'engine_version', 'framework_version', 'arch', 'source', 'rolling_tag')},
                 {k:v for k,v in declaration['versions'][0].items() if k != 'registry_sources'}, 'independent corpus version facts')
            check('registry_sources' not in versions[0], 'empty registry sources must use DTO omission, not null')
        same(sorted(row['id'] for row in listed), sorted([row['id'] for row in self.baseline] + [row['id'] for row in found]),
             'no extra corpus rows')
        return {row['name']: row['id'] for row in found}

    def confirm_gone(self, ident):
        listed = self.listed()
        check(not any(row['id'] == ident for row in listed), 'deleted corpus series remains')
        versions = self.page.request.get(self.base + '/api/v1/images/versions')
        check(versions.status == 200, 'corpus version collection GET failed')
        check(not any(row['series_id'] == ident for row in collection(versions)), 'deleted corpus version remains')
        detail = self.page.request.get(self.base + '/api/v1/images/' + str(ident))
        check(detail.status == 404, 'deleted corpus detail must return exact 404')
        self.owned[:] = [row for row in self.owned if row['id'] != ident]
        self.state.setdefault('cleanup', []).append({'series_id': ident, 'series_absent': True, 'versions_absent': True, 'detail_status': detail.status})

    def cleanup(self):
        errors = []
        for row in list(reversed(self.owned)):
            try:
                response = self.page.request.delete(self.base + '/api/v1/image-series/' + str(row['id']))
                check(response.status == 204, 'corpus cleanup DELETE failed')
                self.confirm_gone(row['id'])
            except Exception as error:
                errors.append({'series_id': row['id'], 'error': str(error)})
        self.state.setdefault('cleanup_errors', []).extend(errors)
        check(not errors, 'corpus cleanup stages failed: ' + repr(errors))
        same(sorted(self.listed(), key=lambda row: row['id']), sorted(self.baseline, key=lambda row: row['id']),
             'original complete fixture restored after corpus cleanup')
        self.state['pending_declared_names'] = []
