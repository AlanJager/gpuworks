"""Independent corpus/query oracle; no DOM or product-response expected values."""
from urllib.parse import parse_qs, urlsplit
from model_metadata_table_owner import require


def expected_slice(rows, owned, page, size, q, source='all'):
    require(size in (10, 20, 50, 100) and page > 0, 'illegal expected page')
    chosen = [r for r in rows if (source == 'all' or r.source == source)
              and (q in r.name or q in r.repo)]
    require([r.number for r in chosen] == sorted(r.number for r in chosen), 'corpus order drift')
    by_repo = {r.repo: ident for ident, r in owned.items()}
    require(len(by_repo) == len(owned), 'duplicate captured identity')
    require(all(r.repo in by_repo for r in chosen), 'missing declared ID binding')
    offset = (page - 1) * size
    return len(chosen), [(by_repo[r.repo], r) for r in chosen[offset:offset + size]]


def query_matches(url, page, size, q, source='all'):
    parts = urlsplit(url)
    wanted = {'page': [str(page)], 'page_size': [str(size)], 'sort': ['name']}
    if q:
        wanted['q'] = [q]
    if source != 'all':
        wanted['source'] = [source]
    return parts.path == '/api/v1/models' and parse_qs(parts.query, keep_blank_values=True) == wanted


def assert_document(document, total, expected, page, size):
    require(document.get('total') == total and document.get('page') == page
            and document.get('page_size') == size, 'wire total/page/size differs')
    items = document.get('items')
    require(isinstance(items, list) and [m.get('id') for m in items] == [i for i, _ in expected],
            'wire ID slice differs from corpus')
    for item, (ident, row) in zip(items, expected):
        require(all(item.get(k) == v for k, v in {'id': ident, 'name': row.name,
                'source_id': row.repo, 'source': row.source, 'revision': row.revision}.items()),
                'wire row identity differs from corpus')


def assert_walk(observed, expected):
    require(observed == expected, 'walk order/repeat/hole differs')
    require(len(observed) == len(set(observed)), 'walk repeats IDs')


def assert_ai(body, status, document, ident, locale):
    require(body == {'task': 'model.fit', 'model_id': ident, 'locale': locale}, 'AI wire scope differs')
    require(status == 503 and document.get('code') == 'AI_ENDPOINT_NOT_CONFIGURED',
            'AI unconfigured contract differs')
