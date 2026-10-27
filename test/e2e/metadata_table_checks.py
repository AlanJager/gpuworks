"""One authoritative client-paged image-series table contract (10.5-.8/.12).

Owns a separate page and declared metadata corpora. No image bytes, agent,
Docker, registry, management interception, or source-derived paginator oracle.
"""
import json
import re
from urllib.parse import urlsplit
from playwright.sync_api import expect
from local_browser_fixture import ROOT
from platform_token_read_revoke import button, check, collection
from metadata_table_fixture import OwnedCorpus, SIZES, PAGE_SIZES, corpus, same, page_names, assert_traversal


def pagination(page):
    result = page.locator('.ant-pagination').filter(visible=True)
    expect(result).to_have_count(1)
    return result


def assert_page(page, declared, identities, size, number, common):
    expected = page_names(declared, number, size)
    rows = page.get_by_role('table').locator('tbody tr[data-row-key]')
    expect(rows).to_have_count(len(expected))
    expect(rows.locator('td').filter(has=page.locator('button.ant-btn-link')).get_by_role('button')).to_have_text(expected)
    same(rows.evaluate_all('es => es.map(e => Number(e.dataset.rowKey))'),
         [identities[name] for name in expected], 'exact visible row IDs/order')
    pager = pagination(page)
    expect(pager.locator('.ant-pagination-item-active')).to_have_attribute('title', str(number))
    expect(pager.locator('.ant-pagination-total-text')).to_have_text(
        common['resourceTable']['total'].replace('{{count}}', str(len(declared))))
    expect(pager.locator('.ant-pagination-prev')).to_have_attribute('aria-disabled', 'true' if number == 1 else 'false')
    expect(pager.locator('.ant-pagination-next')).to_have_attribute('aria-disabled', 'true' if number == (len(declared)+size-1)//size else 'false')
    return expected


def choose_size(page, size):
    pager = pagination(page)
    selector = pager.get_by_role('combobox')
    expect(selector).to_have_count(1)
    # A selected AntD label overlays its search input. Click the actual
    # selector shell, as the existing deploy selector helper does; no force.
    shell = selector.locator("xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-select-selector ')][1]")
    expect(shell).to_have_count(1)
    shell.click()
    expect(selector).to_have_attribute('aria-expanded', 'true')
    popup = page.locator('[id=' + json.dumps(selector.get_attribute('aria-controls')) + ']').locator('..')
    options = popup.locator('.ant-select-item-option')
    # Locale suffix varies; the independent allowed numeric set does not.
    same(options.evaluate_all(r'es => es.map(e => Number(e.title.match(/^\d+/)[0]))'), list(PAGE_SIZES), 'all page-size options')
    check(size in PAGE_SIZES, 'undeclared page size selection')
    options.nth(PAGE_SIZES.index(size)).click()
    expect(selector).to_have_attribute('aria-expanded', 'false')


def sort_name(page, text, direction='ascending'):
    header = page.get_by_role('columnheader', name=text['seriesTable']['column']['series'], exact=True)
    header.get_by_text(text['seriesTable']['column']['series'], exact=True).click()
    expect(header).to_have_attribute('aria-sort', direction)


def refresh(page, base, text, *, initial=False, ledger=None):
    observation = {'document_commits': [], 'requests': [], 'cleanup_errors': []}
    if ledger is not None:
        ledger.setdefault('refreshes', []).append(observation)
    epoch, requests, active = 0, {}, []
    responses = []
    pending_document = False

    def committed(frame):
        nonlocal epoch, pending_document
        if pending_document and frame is page.main_frame and frame.url == base + '/images':
            epoch += 1
            pending_document = False
            observation['document_commits'].append({'epoch': epoch, 'url': frame.url})

    def requested(request):
        nonlocal pending_document
        if (request.frame is page.main_frame and request.is_navigation_request()
                and request.resource_type == 'document'):
            pending_document = True
        if (request.frame is page.main_frame and request.method == 'GET'
                and urlsplit(request.url).path == '/api/v1/images'):
            entry = {'sequence': len(observation['requests']) + 1,
                     'url': request.url, 'epoch': epoch, 'created_after_commit': epoch > 0}
            observation['requests'].append(entry)
            requests[request] = entry

    def belongs_to_document(response):
        entry = requests.get(response.request)
        eligible = entry is not None and epoch > 0 and entry['epoch'] == epoch
        if eligible:
            observation['selected_request_sequence'] = entry['sequence']
        return eligible

    def received(response):
        if belongs_to_document(response):
            responses.append((epoch, response))

    primary = None
    try:
        for event, callback in [('framenavigated', committed), ('request', requested), ('response', received)]:
            page.on(event, callback)
            active.append((event, callback))
        if initial:
            page.goto(base + '/images')
        else:
            page.reload()
        # Auth gates ImagesPage mounting. Its locale title proves the app is
        # ready independently of list data; keep early current-document data.
        expect(page.get_by_role('heading', name=text['page']['title'], exact=True)).to_be_visible()
        observation['heading_visible'] = True
        current = [response for generation, response in responses if generation == epoch]
        if current:
            response = current[0]
            observation['response_phase'] = 'before_heading_ready'
        else:
            with page.expect_response(belongs_to_document) as observed:
                pass
            response = observed.value
            observation['response_phase'] = 'after_heading_ready'
        same({'method': response.request.method, 'path': urlsplit(response.url).path,
              'query': urlsplit(response.url).query, 'body': response.request.post_data},
             {'method': 'GET', 'path': '/api/v1/images', 'query': '', 'body': None}, 'real client-side list GET')
        check(response.status == 200, 'metadata browser refresh failed')
        return collection(response)
    except BaseException as error:
        primary = error
        raise
    finally:
        for event, callback in reversed(active):
            try:
                page.remove_listener(event, callback)
            except Exception as error:
                observation['cleanup_errors'].append(event + ': ' + str(error))
        if observation['cleanup_errors'] and primary is None:
            raise AssertionError('metadata refresh listener cleanup failed: ' + str(observation['cleanup_errors']))


def row_detail(page, server, text, name, ident, total, number, state):
    row = page.locator('tr[data-row-key="' + str(ident) + '"]')
    expect(row).to_have_count(1)
    action = row.get_by_role('button', name=text['seriesTable']['details'], exact=True)
    with page.expect_response(lambda r: r.request.method == 'GET' and
            urlsplit(r.url).path == '/api/v1/images/' + str(ident)) as opened:
        action.click()
    response = opened.value
    same({'method': response.request.method, 'path': urlsplit(response.url).path,
          'query': urlsplit(response.url).query, 'body': response.request.post_data},
         {'method': 'GET', 'path': '/api/v1/images/' + str(ident), 'query': '', 'body': None},
         'cross-page actual detail request')
    check(response.status == 200, 'cross-page browser detail GET failed')
    same({k: response.json()[k] for k in ('id', 'name')}, {'id': ident, 'name': name},
         'cross-page browser detail identity')
    independent = page.request.get(server.base + '/api/v1/images/' + str(ident))
    check(independent.status == 200, 'cross-page independent detail GET failed')
    same({k: independent.json()[k] for k in ('id', 'name')}, {'id': ident, 'name': name},
         'cross-page independent detail identity')
    drawer = page.locator('.ant-drawer-content').filter(visible=True)
    expect(drawer).to_have_count(1)
    expect(drawer.get_by_text(name, exact=True)).to_be_visible()
    drawer.locator('.ant-drawer-close').click()
    expect(drawer).not_to_be_visible()
    state.setdefault('cross_page_actions', []).append({'total': total, 'page': number,
        'id': ident, 'name': name, 'browser_GET': 200, 'independent_GET': 200, 'returned': True})


def selected_page_size(page):
    selected = pagination(page).locator('.ant-select-selection-item')
    expect(selected).to_have_count(1)
    return selected.evaluate(r'e => Number(e.title.match(/^\d+/)[0])')


def restore_baseline_table(page, server, text, baseline, state):
    # Cleanup leaves the last engine/framework intersection empty. A fresh
    # document clears ephemeral filters; select the known nonempty source tab
    # before touching a pager that legitimately does not exist for zero rows.
    original_custom = [row for row in baseline if row['source'] == 'custom']
    check(bool(original_custom), 'metadata restoration needs declared nonempty baseline custom rows')

    def custom_rows():
        page.get_by_role('tab', name=text['page']['tab']['custom'], exact=True).click()
        rows = page.get_by_role('table').locator('tbody tr[data-row-key]')
        expect(rows).to_have_count(len(original_custom))
        same(sorted(rows.evaluate_all('es=>es.map(e=>Number(e.dataset.rowKey))')),
             sorted(row['id'] for row in original_custom), 'original UI fixture restored')

    restored = refresh(page, server.base, text, ledger=state)
    same(sorted(restored, key=lambda row: row['id']), sorted(baseline, key=lambda row: row['id']),
         'original full baseline GET before preference restoration')
    custom_rows()
    choose_size(page, 20)
    reloaded = refresh(page, server.base, text, ledger=state)
    same(sorted(reloaded, key=lambda row: row['id']), sorted(baseline, key=lambda row: row['id']),
         'original full baseline GET after preference reload')
    custom_rows()
    same(selected_page_size(page), 20, 'restored page-size20 persists after reload')
    state['baseline_restoration'] = {'page_size': 20, 'reloaded': True,
        'custom_ids': sorted(row['id'] for row in original_custom), 'collection_count': len(baseline)}


def select_scope(page, text, engine, framework, select_filter, locale, engines, frameworks):
    page.get_by_role('tab', name=text['page']['tab']['custom'], exact=True).click()
    titles = text['seriesTable']['column']
    # Complete options are immutable original fixture facts, supplied by caller.
    select_filter(page, titles['engine'], [engine], engines, locale)
    select_filter(page, titles['framework'], [framework], frameworks, locale)


def geometry(page, text, common, identities, state):
    # All columns fit at default widths. A real keyboard resize creates table
    # overflow without changing viewport or injecting CSS/product DOM state.
    handle = page.get_by_role('separator', name=common['resourceTable']['resizeColumn'].replace('{{name}}', text['seriesTable']['column']['series']), exact=True)
    expect(handle).to_have_attribute('aria-valuenow', '240')
    for _ in range(40):
        handle.press('ArrowRight')
    expect(handle).to_have_attribute('aria-valuenow', '880')
    scroller = page.locator('.ant-table-content')
    expect(scroller).to_have_count(1)
    check(scroller.evaluate('e => e.scrollWidth > e.clientWidth'), 'declared expanded columns did not create table scroll')
    first_name = sorted(identities)[0]
    row = page.locator('tr[data-row-key="' + str(identities[first_name]) + '"]')
    row.scroll_into_view_if_needed()
    action = row.get_by_role('button', name=text['seriesTable']['details'], exact=True)
    for side, delta in [('right', 4000), ('left', -4000)]:
        scroller.hover(position={'x': 10, 'y': 10})
        page.mouse.wheel(delta, 0)
        page.wait_for_function("side => {const e=document.querySelector('.ant-table-content');return side==='left'?e.scrollLeft===0:e.scrollLeft>=e.scrollWidth-e.clientWidth-1}", arg=side)
        expect(action).to_be_in_viewport(ratio=1)
        check(page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'metadata page horizontal overflow')
        box = action.bounding_box()
        state.setdefault('geometry', []).append({'side': side, 'action_box': box,
            'document_width': page.evaluate('document.documentElement.scrollWidth'), 'viewport': 1440})
    ident = identities[first_name]
    with page.expect_response(lambda r: r.request.method == 'GET' and urlsplit(r.url).path == '/api/v1/images/' + str(ident)) as detail:
        action.click()
    check(detail.value.status == 200 and detail.value.json()['id'] == ident, 'fixed action opened wrong metadata row')
    drawer = page.locator('.ant-drawer-content').filter(visible=True)
    expect(drawer).to_have_count(1)
    expect(drawer.get_by_text(first_name, exact=True)).to_be_visible()
    drawer.locator('.ant-drawer-close').click()
    expect(drawer).not_to_be_visible()
    for _ in range(40):
        handle.press('ArrowLeft')
    expect(handle).to_have_attribute('aria-valuenow', '240')


def close_probe(page, state, primary=None):
    try:
        page.close()
        state['page_closed'] = page.is_closed()
        check(state['page_closed'], 'metadata pilot page did not close')
    except Exception as error:
        state['page_close_error'] = str(error)
        if primary is None:
            raise
        state['primary_failure_preserved'] = str(primary)


def exercise(parent, server, artifacts, report, locale, text, engines, frameworks, select_filter, reset_filter):
    report['metadata_table'] = state = {'authoritative_consumer': 'image-catalog-filters',
        'candidate_items': ['10.5','10.6','10.7','10.8','10.12'], 'remaining_resources': [],
        'datasets': [], 'browser_requests': [], 'page_closed': False,
        'scope': 'real image catalogue metadata only; same ImageSeriesTable is not duplicated under enable-cycle'}
    page = parent.context.new_page()
    page.set_default_timeout(12000)
    errors, failures = [], []
    def requested(r):
        path = urlsplit(r.url).path
        if path.startswith(('/api/v1/images', '/api/v1/image-series')):
            state['browser_requests'].append({'method': r.method, 'path': path, 'query': urlsplit(r.url).query, 'body': r.post_data_json if r.post_data else None})
    page.on('request', requested)
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.on('console', lambda m: errors.append(m.text) if m.type == 'error' else None)
    page.on('response', lambda r: failures.append({'path': urlsplit(r.url).path, 'status': r.status}) if r.status >= 400 else None)
    primary = None
    try:
        baseline = collection(page.request.get(server.base + '/api/v1/images'))
        # Original matrix has engineA/all frameworks and other engines/CUDA.
        # engineB/ROCm is a known empty intersection, not a DOM-selected scope.
        engine, framework = engines[1], 'rocm'
        check(not any(row['source']=='custom' and row['engine']==engine and row['framework']==framework for row in baseline), 'pilot scope is not disjoint from original fixture')
        owner = OwnedCorpus(page, server.base, baseline, report)
        common = json.loads((ROOT / f'ui/src/i18n/locales/{locale}/common.json').read_text())
        for total in (*SIZES, 21):
            report['last_step'] = f'metadata-table-dataset-{total}-import'
            prefix = 'e2e-' + report['run_id'].lower() + '-table-' + str(total)
            declared = corpus(prefix, total, engine, framework)
            identities = owner.register(declared)
            names = sorted(identities)
            observed = refresh(page, server.base, text, initial=total == SIZES[0], ledger=state)
            same(sorted(r['id'] for r in observed), sorted([r['id'] for r in baseline]+list(identities.values())), 'browser complete unpaged catalogue corpus')
            select_scope(page, text, engine, framework, select_filter, locale, engines, frameworks)
            sort_name(page, text)
            stage = {'total': total, 'names': names, 'ids': identities, 'traversals': []}
            state['datasets'].append(stage)
            if total == SIZES[0]:
                state['initial_page_size'] = selected_page_size(page)
                same(state['initial_page_size'], 20, 'original ResourceTable default page-size20')
                assert_page(page, names, identities, 20, 1, common)
            if total in SIZES:
                for size in PAGE_SIZES:
                    report['last_step'] = f'metadata-table-{total}-size-{size}'
                    choose_size(page, size)
                    pages = (total+size-1)//size
                    forward = [assert_page(page, names, identities, size, 1, common)]
                    for number in range(2, pages+1):
                        pagination(page).locator('.ant-pagination-next').click()
                        forward.append(assert_page(page, names, identities, size, number, common))
                    backward = [forward[-1]]
                    for number in range(pages-1, 0, -1):
                        pagination(page).locator('.ant-pagination-prev').click()
                        backward.append(assert_page(page, names, identities, size, number, common))
                    assert_traversal(forward, names, size)
                    assert_traversal(backward, names, size, backwards=True)
                    stage['traversals'].append({'size': size, 'forward': forward, 'backward': backward})
                # Stored size survives reload; filters and sort intentionally do not.
                observed = refresh(page, server.base, text, ledger=state)
                select_scope(page, text, engine, framework, select_filter, locale, engines, frameworks)
                sort_name(page, text)
                assert_page(page, names, identities, 100, 1, common)
            if total == 100:
                choose_size(page, 10)
                pagination(page).get_by_title('10', exact=True).click()
                assert_page(page, names, identities, 10, 10, common)
                # Actual size change resets page1; no page-index injection.
                report['last_step'] = 'metadata-table-100-size-page-reset'
                choose_size(page, 20)
                assert_page(page, names, identities, 20, 1, common)
                pagination(page).get_by_title('5', exact=True).click()
                assert_page(page, names, identities, 20, 5, common)
                report['last_step'] = 'metadata-table-100-filter-page-reset'
                select_filter(page, text['seriesTable']['column']['status'], [text['common']['enabled']], [text['common']['enabled'], text['common']['disabled']], locale)
                enabled_names = [name for i,name in enumerate(names) if i%2==0]
                assert_page(page, enabled_names, identities, 20, 1, common)
                reset_filter(page, text['seriesTable']['column']['status'], [text['common']['enabled'], text['common']['disabled']], locale)
                assert_page(page, names, identities, 20, 1, common)
                choose_size(page, 10)
                assert_page(page, names, identities, 10, 1, common)
                report['last_step'] = 'metadata-table-100-horizontal-fixed-actions'
                geometry(page, text, common, identities, state)
            if total in SIZES:
                report['last_step'] = f'metadata-table-{total}-cross-page-detail'
                choose_size(page, 10)
                last_page = (total + 9) // 10
                if last_page > 1:
                    pagination(page).get_by_title(str(last_page), exact=True).click()
                assert_page(page, names, identities, 10, last_page, common)
                target = names[-1]
                row_detail(page, server, text, target, identities[target], total, last_page, state)
                assert_page(page, names, identities, 10, last_page, common)
            if total == 21:
                choose_size(page, 10)
                pagination(page).get_by_title('3', exact=True).click()
                assert_page(page, names, identities, 10, 3, common)
                deleted = names[-1]
                ident = identities[deleted]
                row = page.locator('tr[data-row-key="' + str(ident) + '"]')
                row.get_by_role('button', name=text['common']['delete'], exact=True).click()
                confirm = page.locator('.ant-popconfirm').filter(visible=True)
                expect(confirm).to_have_count(1)
                expect(confirm.get_by_text(text['seriesTable']['deleteTitle'].replace('{{name}}', deleted), exact=True)).to_be_visible()
                with page.expect_response(lambda r: r.request.method == 'DELETE' and urlsplit(r.url).path == '/api/v1/image-series/' + str(ident)) as removed:
                    button(confirm, text['common']['delete']).click()
                same(removed.value.status, 204, 'real last-page delete status')
                owner.confirm_gone(ident)
                assert_page(page, names[:-1], identities, 10, 2, common)
                stage['deleted'] = {'name': deleted, 'id': ident, 'remaining': 20, 'page': 2}
            report['last_step'] = f'metadata-table-{total}-scoped-cleanup'
            owner.cleanup()
        report['last_step'] = 'metadata-table-restore-original-preferences'
        restore_baseline_table(page, server, text, baseline, state)
        expected_actions = [(total, (total + 9) // 10,
            'e2e-' + report['run_id'].lower() + '-table-' + str(total) + f'-{total - 1:03}')
            for total in SIZES]
        same([(entry['total'], entry['page'], entry['name']) for entry in state['cross_page_actions']],
             expected_actions, 'all three real volumes have declared cross-page row actions')
        same(errors, [], 'metadata pilot console/page errors')
        same(failures, [], 'metadata pilot HTTP errors')
        mutations = [r for r in state['browser_requests'] if r['method']!='GET']
        last = state['datasets'][-1]['deleted']
        same(mutations, [{'method':'DELETE','path':'/api/v1/image-series/'+str(last['id']),'query':'','body':None}], 'only real last-page mutation')
        for request in state['browser_requests']:
            if request['path']=='/api/v1/images':
                same(request, {'method':'GET','path':'/api/v1/images','query':'','body':None}, 'client paging never synthesizes server query')
        state['complete'] = True
    except BaseException as error:
        primary = error
        try:
            page.screenshot(path=artifacts.directory / 'metadata-table-failure.png')
            (artifacts.directory / 'metadata-table-failure-aria.txt').write_text(
                artifacts.redact(page.locator('body').aria_snapshot()))
        except Exception as capture_error:
            state['failure_capture_error'] = str(capture_error)
        raise
    finally:
        close_probe(page, state, primary)
