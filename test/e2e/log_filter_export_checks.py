"""Extra real-browser contracts for the warning-filter/export flow.

Expected rows are the independently collected real WARN facts. All search
responses are captured by the caller before the browser action, never mocked.
"""
import re
from urllib.parse import parse_qs, urlencode, urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import button, check


def text_inventory(page, logs, report):
    report['last_step'] = 'logs-text-inventory'
    fields = (
        page.get_by_placeholder(logs['search']['placeholder'], exact=True),
        page.locator('.ant-select-auto-complete input[role="combobox"]'),
        page.get_by_role('combobox', name=logs['search']['nodePlaceholder'], exact=True),
    )
    for field, placeholder in zip(fields, (
            logs['search']['placeholder'], logs['search']['instancePlaceholder'],
            logs['search']['nodePlaceholder'])):
        expect(field).to_be_visible(timeout=12000)
        expect(field).to_be_enabled()
        expect(field).to_have_value('')
        if field is fields[0]:
            expect(field).to_have_attribute('placeholder', placeholder)
        else:
            wrapper = field.locator('xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-select ")][1]')
            expect(wrapper.locator('.ant-select-selection-placeholder')).to_have_text(placeholder)
        check(field.get_attribute('readonly') is None, 'log text control is read-only')
        check(field.get_attribute('required') is None, 'log text control is required')
        check(field.get_attribute('maxlength') is None, 'log text control has an undeclared maximum')
    # AntD's leaving loading icon contributes "loading" to the accessible
    # name. Match the real button's precise visible localized text instead.
    caption = logs['search']['button']
    pattern = r'\s*'.join(caption) if re.fullmatch(r'[\u3400-\u9fff]{2}', caption) else re.escape(caption)
    expect(page.get_by_role('button').filter(has_text=re.compile('^' + pattern + '$'))).to_be_enabled()
    report['text_inventory'] = {
        'query': {'placeholder': logs['search']['placeholder'], 'default': '', 'optional': True},
        'instance': {'placeholder': logs['search']['instancePlaceholder'], 'default': '', 'optional': True},
        'node_search': {'placeholder': logs['search']['nodePlaceholder'], 'default': '', 'optional': True},
        'readonly': False, 'disabled': False, 'html_maxlength': None,
        'persistence': 'transient filters, not stored configuration',
    }


def option_inventory(page, logs, report):
    """All source, severity and range choices; no invented clear operation."""
    report['last_step'] = 'logs-selector-inventory'
    cases = (
        (logs['search']['sourceLabel'], [logs['search'][key] for key in (
            'sourceInstance', 'sourceNode', 'sourceAgent', 'sourceServer', 'sourceAll')],
         logs['search']['sourceInstance']),
        (logs['search']['levelPlaceholder'], ['≥ info', '≥ warn', '≥ error', '≥ fatal'], None),
        (logs['search']['rangeLabel'], [logs['range'][key] for key in (
            '30m', '1h', '6h', '24h', '7d', 'all', 'custom')], logs['range']['24h']),
    )
    observed = []
    for label, labels, selected in cases:
        control = page.get_by_role('combobox', name=label, exact=True)
        expect(control).to_be_enabled()
        wrapper = control.locator('xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-select ")][1]')
        if selected is not None:
            expect(wrapper.locator('.ant-select-selection-item')).to_have_text(selected)
        else:
            expect(wrapper.locator('.ant-select-selection-placeholder')).to_have_text(label)
            expect(wrapper.locator('.ant-select-selection-item')).to_have_count(0)
        control.press('ArrowDown')
        expect(control).to_have_attribute('aria-expanded', 'true')
        options = page.locator('.ant-select-dropdown:visible .ant-select-item-option')
        expect(options).to_have_count(len(labels), timeout=12000)
        for index, value in enumerate(labels):
            expect(options.nth(index)).to_have_attribute('title', value)
            expect(options.nth(index)).to_have_text(value)
            expect(options.nth(index)).not_to_have_class(re.compile(r'\bant-select-item-option-disabled\b'))
        control.press('Escape')
        expect(control).to_have_attribute('aria-expanded', 'false')
        expect(control).to_be_focused()
        observed.append({'name': label, 'default': selected, 'choices': labels, 'disabled': False})
    report['selector_inventory'] = observed


def exercise_text_inputs(page, context, server, logs, report, selected, target, result):
    """Independently isolate normalization and real clipboard diagnostics."""
    query = selected['q']
    search = page.get_by_placeholder(logs['search']['placeholder'], exact=True)
    instance = page.locator('.ant-select-auto-complete input[role="combobox"]')
    node = page.get_by_role('combobox', name=logs['search']['nodePlaceholder'], exact=True)
    without_query = {key: value for key, value in selected.items() if key != 'q'}
    family = report.get('check_family', 'all')
    observed = {'family': family, 'multiline': 'not applicable: these controls are single-line'}
    mutations = []

    def sent(request):
        if request.method in ('POST', 'PUT', 'PATCH', 'DELETE'):
            mutations.append({'method': request.method, 'path': urlsplit(request.url).path})

    def enter(value):
        search.fill(value)
        search.press('Enter')

    # The instance and node filters are empty at entry and exit. Each family
    # therefore starts from the same actual core-flow query, including when
    # whitespace and clipboard regressions coexist in one product binary.
    expect(search).to_have_value(query)
    expect(instance).to_have_value('')
    expect(node).to_have_value('')

    # A NodePicker edit queries candidates, not logs. The fresh fixture owns
    # no nodes, so its precise candidate set is empty after every text edit.
    def node_query(action, expected):
        def matches(response):
            return (response.request.method == 'GET' and urlsplit(response.url).path == '/api/v1/nodes'
                    and parse_qs(urlsplit(response.url).query) == expected)
        with page.expect_response(matches) as received:
            action()
        response = received.value
        value = response.json()
        check(response.status == 200 and value['items'] == [] and value['total'] == 0,
              'node candidate search returned an unexpected fixture resource')
        report['evidence'].append({'layer': 'request', 'scenario': 'node text edit',
            'method': 'GET', 'path': '/api/v1/nodes', 'query': expected, 'response': value})

    node_page = {'page': ['1'], 'page_size': ['50']}
    page.on('request', sent)
    try:
        if family in ('all', 'text-inputs', 'whitespace'):
            report['last_step'] = 'logs-query-whitespace'
            for value, expected, label in (
                    ('', without_query, 'empty query is omitted'),
                    ('   ', without_query, 'whitespace-only query is omitted'),
                    ('  ' + query + '  ', selected, 'query boundaries trim on wire')):
                result(lambda value=value: enter(value), expected, [target], label)
                expect(search).to_have_value(value)
            # Instance prefixes preserve whitespace, unlike buildQuery.
            report['last_step'] = 'logs-instance-whitespace'
            for value in ('   ', ' e2e-no-such-instance ', 'e2e-no-such-instance'):
                result(lambda value=value: instance.fill(value), {**selected, 'instance': value}, [],
                       'instance prefix preserves ' + repr(value))
                expect(instance).to_have_value(value)
            result(lambda: instance.fill(''), selected, [target], 'empty instance omitted')
            report['last_step'] = 'logs-node-search-whitespace'
            node_query(lambda: node.fill('  e2e-no-such-node  '), {**node_page, 'q': ['e2e-no-such-node']})
            node_query(lambda: node.fill('   '), node_page)
            node_query(lambda: node.fill(''), node_page)
            node.press('Escape')
            result(lambda: enter(query), selected, [target], 'normalization family restores exact query')
            observed.update(query_trimmed=True, instance_whitespace_preserved=True,
                            node_query_trimmed=True, empty_omitted=['query', 'instance', 'node search'])

        if family in ('all', 'text-inputs', 'clipboard'):
            context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=server.base)

            def paste(control, value):
                page.evaluate('(value) => navigator.clipboard.writeText(value)', value)
                control.focus()
                control.press('Control+A')
                control.press('Control+V')

            report['last_step'] = 'logs-query-keyboard-paste'
            result(lambda: search.press('Control+A') or search.press('Backspace'), without_query, [target],
                   'keyboard clears free-text query')
            expect(search).to_have_value('')
            result(lambda: paste(search, query), selected, [target], 'clipboard paste reaches query wire')
            expect(search).to_have_value(query)
            report['last_step'] = 'logs-instance-keyboard-paste'
            result(lambda: paste(instance, 'e2e-no-such-instance'),
                   {**selected, 'instance': 'e2e-no-such-instance'}, [], 'clipboard paste reaches instance wire')
            expect(instance).to_have_value('e2e-no-such-instance')
            result(lambda: instance.press('Control+A') or instance.press('Backspace'), selected, [target],
                   'keyboard clears instance filter')
            report['last_step'] = 'logs-node-search-keyboard'
            node_query(lambda: paste(node, 'e2e-no-such-node'), {**node_page, 'q': ['e2e-no-such-node']})
            node_query(lambda: node.press('Control+A') or node.press('Backspace'), node_page)
            node.press('Escape')
            observed.update(paste=['query', 'instance', 'node search'],
                            keyboard_clear=['query', 'instance', 'node search'])

        expect(search).to_have_value(query)
        expect(instance).to_have_value('')
        expect(node).to_have_value('')
        check(mutations == [], 'read-only text edits sent a mutation')
        response = page.request.get(server.base + '/api/v1/logs/search?' + urlencode(selected))
        check(response.status == 200 and response.json() == {'items': [target], 'total': 1},
              'text diagnostics changed the authoritative warning or filter scope')
        observed.update(filters_restored=True, mutations=mutations)
        report['text_editing'] = observed
    finally:
        page.remove_listener('request', sent)


def exercise_inclusive_boundaries(page, server, logs, report, target, query, result, choose):
    """The real warning is included at each endpoint and excluded one ms away."""
    stamp = target['time_ms']
    cases = (
        ('inclusive-lower', stamp, stamp + 1, [target]),
        ('inclusive-upper', stamp - 1, stamp, [target]),
        ('excluded-before-lower', stamp + 1, stamp + 2, []),
        ('excluded-after-upper', stamp - 2, stamp - 1, []),
    )
    observations = []
    for label, lower, upper, expected in cases:
        report['last_step'] = 'logs-time-' + label
        base = {'source': 'instance', 'from': str(lower), 'to': str(upper), 'limit': '500', 'with_total': '1'}
        result(lambda: page.goto(server.base + '/logs?' + urlencode({'from': lower, 'to': upper})),
               base, [], label + ' opens default instance source')
        search = page.get_by_placeholder(logs['search']['placeholder'], exact=True)
        result(lambda: search.fill(query), {**base, 'q': query}, [], label + ' query')
        actual = result(lambda: choose(page, logs['search']['sourceLabel'], logs['search']['sourceServer']),
               {**base, 'source': 'server', 'q': query}, expected, label)
        response = page.request.get(server.base + '/api/v1/logs/search?' + urlencode(
            {**base, 'source': 'server', 'q': query}))
        check(response.status == 200 and response.json() == actual, 'boundary independent GET differs')
        observations.append({'case': label, 'from': lower, 'to': upper,
                             'expected_ids': [row['id'] for row in expected]})
    report['inclusive_boundaries'] = observations
