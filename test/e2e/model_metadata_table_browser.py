"""UNAPPLIED ModelsPage adapter. Real UI/wire only; browser execution pending."""
import json
import math
from urllib.parse import urlencode, urlsplit
from playwright.sync_api import expect
from model_detail_dialog import model_detail_dialog
from model_hub_registration_browser import texts, poll
from platform_token_read_revoke import button
from model_metadata_table_owner import MODELS, require
from model_metadata_table_oracle import expected_slice, query_matches, assert_document, assert_walk, assert_ai, assert_session_activity, assert_preview_mutations


class ModelsTable:
    def __init__(self, page, server, owner, locale, report):
        self.page, self.server, self.owner = page, server, owner
        self.locale, self.report = locale, report
        self.labels, self.common, self.ai = (texts(locale, n) for n in ('models', 'common', 'ai'))
        self.rows, self.owned = (), {}
        self.size, self.number, self.q, self.source = 20, 1, owner.namespace, 'all'
        self.report.update(pages=[], operations=[], geometry=[], responses=[], page_errors=[], console_errors=[], request_failures=[])
        page.on('pageerror', lambda e: report['page_errors'].append(str(e)))
        page.on('console', lambda m: report['console_errors'].append({'text': m.text, 'url': m.location.get('url', '')}) if m.type == 'error' else None)
        page.on('response', self.response)
        page.on('requestfailed', lambda r: report['request_failures'].append({'url': r.url, 'failure': r.failure}))

    def response(self, response):
        path = urlsplit(response.url).path
        if path.startswith('/api/v1/'):
            event = {'method': response.request.method, 'url': response.url, 'status': response.status}
            if path == '/api/v1/auth/activity':
                event['post_data'] = response.request.post_data
            self.report['responses'].append(event)

    @property
    def table(self):
        return self.page.locator('.model-resource-card .resource-table')

    @property
    def pager(self):
        return self.table.locator('.ant-pagination')

    def expected(self, page=None):
        return expected_slice(self.rows, self.owned, page or self.number, self.size, self.q, self.source)

    def settled(self, response, number=None):
        if number is not None:
            self.number = number
        total, expected = self.expected()
        require(response.status == 200, 'table GET failed')
        require(query_matches(response.url, self.number, self.size, self.q, self.source), 'table query differs')
        document = response.json()
        self.report.setdefault('wire_documents', []).append({'url': response.url, 'body': document})
        assert_document(document, total, expected, self.number, self.size)
        # Separate authenticated management read confirms the declared corpus slice.
        status, independent = self.owner.call('GET', MODELS + '?' + urlencode(
            {'page': self.number, 'page_size': self.size, 'q': self.q, 'sort': 'name',
             **({'source': self.source} if self.source != 'all' else {})}))
        require(status == 200, 'independent table read failed')
        assert_document(independent, total, expected, self.number, self.size)
        rows = self.table.locator('tr[data-row-key]')
        expect(rows).to_have_count(len(expected))
        for index, (ident, row) in enumerate(expected):
            actual = rows.nth(index)
            expect(actual).to_have_attribute('data-row-key', str(ident))
            expect(actual.locator('.model-name-link')).to_have_text(row.name)
            expect(actual).to_contain_text(row.repo)
        expect(self.pager.locator('.ant-pagination-item-active')).to_have_attribute('title', str(self.number))
        self.report['pages'].append({'query': response.url, 'total': total, 'ids': [i for i, _ in expected]})
        return [i for i, _ in expected]

    def transition(self, action, number=1):
        # Bind to a newly created exact request, then its own response. An already
        # inflight polling response cannot satisfy this action's request waiter.
        with self.page.expect_request(lambda r: r.method == 'GET'
                and query_matches(r.url, number, self.size, self.q, self.source), timeout=5000) as requested:
            action()
        response = requested.value.response()
        require(response is not None and response.request == requested.value, 'action response not bound to request')
        return self.settled(response, number)

    def go(self, number, direction=None):
        if number == self.number:
            return [i for i, _ in self.expected()[1]]
        control = (self.pager.locator('.ant-pagination-' + direction) if direction
                   else self.pager.locator(f'.ant-pagination-item[title="{number}"]'))
        return self.transition(control.click, number)

    def size_to(self, size):
        if size == self.size:
            return
        control = self.pager.locator('.ant-pagination-options-size-changer').get_by_role('combobox')
        shell = control.locator("xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-select-selector ')][1]")
        expect(shell).to_have_count(1)
        shell.click()
        expect(control).to_have_attribute('aria-expanded', 'true')
        popup = self.page.locator('[id=' + json.dumps(control.get_attribute('aria-controls')) + ']').locator('..')
        options = popup.locator('.ant-select-item-option')
        labels = options.all_text_contents()
        import re
        require([int(re.search(r'\d+', label).group()) for label in labels] == [10, 20, 50, 100],
                'actual page-size options differ')
        self.size = size
        self.transition(lambda: options.nth((10, 20, 50, 100).index(size)).click())

    def search(self, query, number=1):
        self.q = query
        self.transition(lambda: self.page.locator('#model-search').fill(query), number)

    def source_to(self, source):
        self.source = source
        label = {'all': self.labels['filter']['allSources'], 'huggingface': 'HuggingFace', 'modelscope': 'ModelScope'}[source]
        control = self.page.get_by_role('combobox', name=self.labels['filter']['sourceAria'], exact=True)
        shell = control.locator("xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' ant-select-selector ')][1]")
        def choose():
            # A selected label spans the showSearch input and intercepts pointer
            # events (readiness-05); the user clicks the visible selector body.
            shell.click()
            popup_id = control.get_attribute('aria-controls')
            require(bool(popup_id), 'source selector has no controlled popup')
            popup = self.page.locator('[id=' + json.dumps(popup_id) + ']').locator('..')
            popup.get_by_text(label, exact=True).filter(visible=True).click()
        self.transition(choose)

    def open(self):
        self.page.goto(self.server.base + '/models')
        self.page.get_by_role('img', name=self.labels['view']['list'], exact=True).click()
        expect(self.table).to_be_visible()
        # Precondition only: do not inject/clear a React or preference state.
        expect(self.pager.locator('.ant-pagination-options-size-changer')).to_contain_text('20')
        self.page.locator('#model-search').fill(self.q)
        self.transition(lambda: self.table.get_by_role('columnheader', name=self.labels['column']['name'], exact=True).click())

    def remount(self):
        # Only the page size is persisted; query/sort/page are real component state.
        # Observe the real default request before reapplying controls after refresh.
        with self.page.expect_response(lambda r: r.request.method == 'GET'
                and urlsplit(r.url).path == MODELS
                and r.url.endswith('page=1&page_size=' + str(self.size))) as initial:
            self.page.reload()
        require(initial.value.status == 200, 'refresh initial model GET failed')
        expect(self.pager.locator('.ant-pagination-options-size-changer')).to_contain_text(str(self.size))
        self.number, self.source = 1, 'all'
        self.page.locator('#model-search').fill(self.q)
        self.transition(lambda: self.table.get_by_role('columnheader', name=self.labels['column']['name'], exact=True).click())

    def boundary(self):
        total, _ = self.expected()
        last = max(1, math.ceil(total / self.size))
        expect(self.pager.locator('.ant-pagination-prev')).to_have_attribute('aria-disabled', str(self.number == 1).lower())
        expect(self.pager.locator('.ant-pagination-next')).to_have_attribute('aria-disabled', str(self.number == last).lower())

    def walk(self, operations=False):
        total, _ = self.expected()
        last = max(1, math.ceil(total / self.size))
        self.go(1)
        expected_ids = [i for p in range(1, last + 1) for i, _ in self.expected(p)[1]]
        forward, reverse = [], []
        for number in range(1, last + 1):
            forward += self.go(number, 'next' if number > 1 else None)
            self.boundary()
            if operations:
                # Two representatives sample both source branches on each page.
                for ident, row in self.expected()[1][:2]:
                    self.operations(ident, row)
        for number in range(last, 0, -1):
            ids = self.go(number, 'prev' if number < last else None)
            reverse = ids + reverse
            self.boundary()
        assert_walk(forward, expected_ids)
        assert_walk(reverse, expected_ids)
        if last > 1:
            self.go(last)  # Actual numbered last and first controls.
            self.go(1)

    def visible(self, target):
        # Explicit vertical user navigation; horizontal clipping is never autoscrolled away.
        box = target.bounding_box()
        require(box is not None, 'action has no bounds')
        if box['y'] < 0 or box['y'] + box['height'] > 900:
            self.page.mouse.wheel(0, box['y'] - 400)
        poll(self.page, lambda: target.evaluate('''e => {
            const b=e.getBoundingClientRect(), t=e.closest('.ant-table-container').getBoundingClientRect();
            const hit=document.elementFromPoint(b.x+b.width/2,b.y+b.height/2);
            return b.width>0 && b.x>=Math.max(0,t.x)-1 && b.right<=Math.min(innerWidth,t.right)+1
                && b.y>=0 && b.bottom<=innerHeight && (hit===e || e.contains(hit));
        }'''), 'unclipped reachable action')

    def close(self, dialog):
        dialog.locator('.ant-modal-close').click()
        expect(dialog).not_to_be_visible()
        self.unchanged_table()

    def unchanged_table(self):
        expected = self.expected()[1]
        actual = self.table.locator('tr[data-row-key]')
        expect(actual).to_have_count(len(expected))
        require(actual.evaluate_all('(rows) => rows.map(r => Number(r.dataset.rowKey))') == [i for i, _ in expected],
                'row operation changed table query/page/IDs')
        expect(self.pager.locator('.ant-pagination-item-active')).to_have_attribute('title', str(self.number))

    def row_action(self, ident, label):
        target = self.table.locator(f'tr[data-row-key="{ident}"]').get_by_role('button', name=label, exact=True)
        self.visible(target)
        target.click()

    def detail(self, ident, row):
        target = self.table.locator(f'tr[data-row-key="{ident}"] .model-name-link')
        self.visible(target)
        with self.page.expect_response(lambda r: r.request.method == 'GET' and urlsplit(r.url).path == MODELS + '/' + str(ident)) as read:
            target.click()
        require(read.value.status == 200, 'detail GET failed')
        self.owner.facts(read.value.json(), row)
        dialog = model_detail_dialog(self.page, self.labels, row.name)
        expect(dialog).to_be_visible()
        for value in (row.repo, row.revision, 'Qwen2ForCausalLM', 'float16', '4,096', 'Audited browser registration fixture.'):
            expect(dialog).to_contain_text(value)
        self.close(dialog)

    def operations(self, ident, row):
        before = len(self.report['responses'])
        self.detail(ident, row)
        for action, title, submit in [('downloadToNode', 'download', 'action'), ('copyTo', 'copy', 'start')]:
            with self.page.expect_response(lambda r: r.request.method == 'GET' and urlsplit(r.url).path == MODELS + '/' + str(ident)) as read:
                self.row_action(ident, self.labels['action'][action])
            require(read.value.status == 200, 'action model GET failed')
            self.owner.facts(read.value.json(), row)
            dialog = self.page.get_by_role('dialog', name=self.labels[title]['title'].replace('{{name}}', row.name), exact=True)
            expect(dialog).to_be_visible()
            expect(button(dialog, self.labels[title][submit])).to_be_disabled()
            if title == 'copy':
                expect(dialog).to_contain_text(self.labels['copy']['noSources'])
            else:
                nodes = dialog.get_by_role('combobox', name=self.labels['download']['nodes'], exact=True)
                if nodes.count():
                    nodes.click()
                    expect(self.page.locator('.ant-select-dropdown:visible')).to_contain_text(self.labels['download']['noOnlineNodes'])
                    self.page.keyboard.press('Escape')
                else:
                    expect(dialog).to_contain_text(self.labels['download']['noWritableStorage'])
            self.close(dialog)
        self.ai_action(ident)
        self.row_action(ident, self.labels['action']['more'])
        with self.page.expect_response(lambda r: r.request.method == 'GET' and urlsplit(r.url).path == MODELS + '/' + str(ident)) as read:
            self.page.get_by_role('menuitem').filter(
                has=self.page.get_by_text(self.labels['action']['registerExistingPath'], exact=True)).click()
        require(read.value.status == 200, 'path model GET failed')
        self.owner.facts(read.value.json(), row)
        dialog = self.page.get_by_role('dialog', name=self.labels['path']['title'].replace('{{name}}', row.name), exact=True)
        node = dialog.get_by_role('combobox', name=self.labels['detail']['node'], exact=True)
        node.click()
        expect(self.page.locator('.ant-select-dropdown:visible')).to_contain_text(self.labels['download']['noOnlineNodes'])
        self.page.keyboard.press('Escape')
        expect(button(dialog, self.labels['path']['register'])).to_be_disabled()
        expect(button(dialog, self.labels['path']['listDirectory'])).to_be_disabled()
        self.close(dialog)
        self.delete_preview(ident, row)
        assert_preview_mutations(self.report['responses'][before:], self.server.base)
        self.report['operations'].append({'id': ident, 'source': row.source, 'page': self.number,
            'types': ['detail', 'download', 'copy', 'AI-unconfigured', 'register-path', 'delete-cancel']})

    def ai_action(self, ident):
        status, available = self.owner.call('GET', '/api/v1/ai/available')
        require(status == 200 and available.get('available') is False, 'AI must actually be unavailable')
        with self.page.expect_response(lambda r: r.request.method == 'GET' and urlsplit(r.url).path == f'/api/v1/ai/models/{ident}/fit') as fit:
            with self.page.expect_response(lambda r: r.request.method == 'POST' and urlsplit(r.url).path == '/api/v1/ai/ask') as ask:
                self.row_action(ident, self.ai['task']['model.fit']['action'])
        require(fit.value.status == 200, 'deterministic fit GET failed')
        body, document = ask.value.request.post_data_json, ask.value.json()
        self.report.setdefault('ai_wire', []).append({'request': body, 'status': ask.value.status, 'response': document,
                                                      'fit_status': fit.value.status, 'fit': fit.value.json()})
        assert_ai(body, ask.value.status, document, ident, self.locale)
        panel = self.page.locator('.assistant-panel')
        expect(panel.get_by_text(self.ai['configureFirst'], exact=False)).to_be_visible()
        panel.get_by_role('button', name=self.ai['panel']['close'], exact=True).click()
        self.unchanged_table()
        self.report.setdefault('ai', []).append({'id': ident, 'status': 503, 'code': 'AI_ENDPOINT_NOT_CONFIGURED'})

    def delete_preview(self, ident, row, confirm=False):
        self.row_action(ident, self.labels['action']['more'])
        self.page.get_by_role('menuitem').filter(
                has=self.page.get_by_text(self.labels['action']['deleteModel'], exact=True)).click()
        dialog = self.page.get_by_role('dialog', name=self.labels['delete']['confirm'].replace('{{name}}', row.name), exact=True)
        expect(dialog).to_be_visible()
        if not confirm:
            button(dialog, self.labels['action']['cancel']).click()
            self.unchanged_table()
        else:
            with self.page.expect_response(lambda r: r.request.method == 'DELETE' and urlsplit(r.url).path == MODELS + '/' + str(ident)) as deleted:
                button(dialog, self.labels['action']['delete']).click()
            require(deleted.value.status == 204, 'singleton UI DELETE failed')
        expect(dialog).not_to_be_visible()

    def geometry(self):
        require(self.page.viewport_size == {'width': 1440, 'height': 900}, 'formal viewport differs')
        handle = self.table.get_by_role('separator').first
        self.visible(handle)
        old = int(handle.get_attribute('aria-valuenow'))
        primary = None
        try:
            handle.focus()
            self.page.keyboard.press('ArrowRight')
            expect(handle).to_have_attribute('aria-valuenow', str(old + 16))
            scroll = self.table.locator('.ant-table-content')
            def measure():
                return scroll.evaluate('''e => ({left:e.scrollLeft, client:e.clientWidth, width:e.scrollWidth,
                    document:document.documentElement.scrollWidth,body:document.body.scrollWidth,viewport:innerWidth})''')
            measurements = [measure()]
            require(measurements[0]['width'] > measurements[0]['client'] + 1,
                    '10.12 requires actual internal table overflow')
            for delta in (10000, -10000):
                box = scroll.bounding_box()
                require(box is not None, 'internal scroll region has no bounds')
                self.page.mouse.move(box['x'] + box['width']/2, max(100, min(800, box['y'] + 100)))
                self.page.mouse.wheel(delta, 0)
                poll(self.page, lambda: abs(measure()['left'] - (measure()['width'] - measure()['client'] if delta > 0 else 0)) < 2,
                     'native internal horizontal scroll endpoint')
                measurements.append(measure())
                ident, row = self.expected()[1][0]
                cell = self.table.locator(f'tr[data-row-key="{ident}"] .resource-table-actions-cell')
                sticky = cell.evaluate('''e => {const b=e.getBoundingClientRect(),t=e.closest('.ant-table-container').getBoundingClientRect();
                    return {position:getComputedStyle(e).position,left:b.left,right:b.right,clipLeft:t.left,clipRight:t.right};}''')
                require(sticky['position'] == 'sticky' and sticky['left'] >= max(0,sticky['clipLeft'])-1
                        and sticky['right'] <= min(1440,sticky['clipRight'])+1, 'fixed action cell escapes table/viewport')
                self.report.setdefault('sticky_actions', []).append({'id': ident, **sticky})
                # Concrete fixed-column operations bind realGET/details/modals to this
                # declared ID at each edge; delete is cancelled, never silently skipped.
                self.operations(ident, row)
                after = measure()
                require(abs(after['left'] - measurements[-1]['left']) < 2,
                        'row actions moved table away from tested native scroll endpoint')
                measurements.append(after)
            require(measurements[1]['left'] > measurements[3]['left'] + 1, 'no real internal scroll occurred')
            require(all(m['document'] <= 1441 and m['body'] <= 1441 for m in measurements),
                    'document horizontally overflows')
            self.report['geometry'].append({'measurements': measurements, 'internal_applicable': True})
        except Exception as error:
            primary = error
            raise
        finally:
            try:
                self.visible(handle)
                handle.focus()
                current = int(handle.get_attribute('aria-valuenow'))
                if current != old:
                    require(current == old + 16, 'unexpected column width during restore')
                    self.page.keyboard.press('ArrowLeft')
                expect(handle).to_have_attribute('aria-valuenow', str(old))
                self.report.setdefault('geometry_restore', []).append({'column_width': old, 'restored': True})
            except Exception as error:
                self.report.setdefault('geometry_restore_error', []).append(type(error).__name__ + ': ' + str(error))
                if primary is None:
                    raise

    def checkpoint(self, count, rows, owned):
        self.rows, self.owned = rows, owned
        if count == 10:
            self.open()
        else:
            self.remount()
        # Each real dataset, including10 and30, independently walks all four sizes.
        for size in (10, 20, 50, 100):
            self.size_to(size)
            self.walk()
            self.report.setdefault('size_matrix', []).append({'count': count, 'size': size})
        self.size_to(20 if count == 100 else 10)
        self.walk(operations=True)  # Preserve the original six-action page matrix.
        self.size_to(10)
        if count != 100:
            return
        self.go(5)
        self.search(self.owner.namespace + '-model-00')
        # usePagedList keeps the page chosen under a filter set: searching away did
        # not re-record paging, so returning to the namespace filters restores the
        # page 5 chosen above; it does not reset to page 1 (readiness-04 evidence).
        self.search(self.owner.namespace, 5)
        self.go(5)
        self.source_to('huggingface')
        self.go(3)
        self.source_to('modelscope')
        self.source_to('all')
        self.go(5)
        self.size_to(20)
        self.remount()
        self.geometry()
        self.size_to(10)
        # Genuine API deletes of exactly declared 092..100. Never modify owned ledger.
        for ident, row in list(owned.items()):
            if row.number >= 92:
                require(self.owner.call('DELETE', MODELS + '/' + str(ident))[0] == 204, 'tail DELETE failed')
                require(self.owner.call('GET', MODELS + '/' + str(ident))[0] == 404, 'tail DELETE absence failed')
        self.rows = rows[:91]
        self.remount()
        self.go(10)
        ident, row = self.expected()[1][0]
        require(row.number == 91 and len(self.expected()[1]) == 1, 'page10 is not declared singleton091')
        self.rows = rows[:90]
        # Attach BEFORE UI DELETE; no page9 click, SSE fabrication, or callback simulation.
        self.transition(lambda: self.delete_preview(ident, row, confirm=True), 9)
        require(self.owner.call('GET', MODELS + '/' + str(ident))[0] == 404, 'singleton independent absence failed')
        require([r.number for _, r in self.expected()[1]] == list(range(81, 91)), 'clamped page9 corpus differs')
        # Success only: retain the strict automatic page9 evidence above, then
        # restore the original default20 through real controls and an actual reload.
        self.size_to(20)
        self.remount()
        require(self.q == self.owner.namespace and self.source == 'all' and self.number == 1 and self.size == 20,
                'success restore query/source/page/size differs')
        remaining = self.owner.listing(self.owner.namespace)
        _, expected = expected_slice(self.rows, self.owned, 1, 100, self.q, self.source)
        require(len(remaining) == 90 and {m['id'] for m in remaining} == {i for i, _ in expected},
                'final independent90 ID set differs from declared remaining corpus')
        by_id = {ident: row for ident, row in expected}
        require(all(m['name'] == by_id[m['id']].name and m['source_id'] == by_id[m['id']].repo
                    and m['source'] == by_id[m['id']].source and m['revision'] == by_id[m['id']].revision
                    for m in remaining), 'final independent90 names/repos differ')
        self.report['success_restore'] = {'page': 1, 'page_size': 20, 'total': 90,
                                          'ids': [i for i, _ in self.expected()[1]]}

    def verify_errors(self):
        require(not self.report['page_errors'], 'table pageerror')
        # Reload/close aborts the three long-lived or mount-time requests; that is
        # normal navigation teardown, not a product failure. Any other request
        # failure, or a non-abort failure on these paths, remains fatal.
        abortable = ('/api/v1/events', '/api/v1/auth/activity', '/api/v1/auth/me')
        for event in self.report['request_failures']:
            require(event['failure'] == 'net::ERR_ABORTED'
                    and urlsplit(event['url']).path in abortable, 'table request failed')
        for event in self.report['responses']:
            if urlsplit(event['url']).path == '/api/v1/auth/activity':
                assert_session_activity(event, self.server.base)
            if event['status'] >= 400:
                require(event['method'] == 'POST' and urlsplit(event['url']).path == '/api/v1/ai/ask'
                        and event['status'] == 503, 'unexpected new-page HTTP error')
        for event in self.report['console_errors']:
            require(urlsplit(event['url']).path == '/api/v1/ai/ask' and '503' in event['text'], 'unexpected new-page console error')
