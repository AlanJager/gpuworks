"""Strict offline helper selftest. No product/browser PASS or management fixture.

These doubles model only route hold/release ownership. They do not simulate a
GPUWorks API or claim that any management endpoint has been executed.
"""
from contextlib import contextmanager
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from template_control_checks import submit_real

URL = 'http://127.0.0.1/api/v1/inference-templates/import?overwrite=true'
BODY = 'models:\n  - model_id: e2e-test/template\n'


class Route:
    def __init__(self, page):
        self.page = page
        self.request = SimpleNamespace(method='POST', url=URL)
        self.released = False
        self.attempts = 0

    def continue_(self):
        self.attempts += 1
        if self.page.release_fails:
            raise TimeoutError('held route release failed')
        if self.released:
            raise AssertionError('route released twice')
        self.released = True
        self.page.delivered += 1
        self.page.button.loading = False


class Page:
    def __init__(self, *, duplicate=False, release_fails=False, status=400, body=BODY):
        self.routes, self.ids = [], []
        self.handler = None
        self.duplicate, self.release_fails = duplicate, release_fails
        self.status, self.body = status, body
        self.delivered = 0
        self.removed = False
        self.button = Button(self)

    def route(self, pattern, handler):
        if self.handler or not pattern.search(URL):
            raise AssertionError('unexpected barrier registration')
        self.handler = handler

    def unroute(self, pattern, handler):
        if handler is not self.handler or not pattern.search(URL):
            raise AssertionError('removing wrong barrier')
        self.handler = None
        self.removed = True

    @contextmanager
    def expect_response(self, predicate):
        response = SimpleNamespace(status=self.status,
            url=URL, request=SimpleNamespace(method='POST', headers={'content-type': 'application/yaml'}))
        if not predicate(response):
            raise AssertionError('wrong response predicate')
        yield SimpleNamespace(value=response)
        if self.delivered != 1:
            raise AssertionError('response cannot precede real route release')

    def send(self, method, params):
        if method != 'Network.getRequestPostData' or params != {'requestId': self.ids[-1]}:
            raise AssertionError('unexpected CDP command/request ID')
        return {'postData': self.body}


class Button:
    def __init__(self, page):
        self.page, self.loading = page, False

    def click(self):
        if self.loading and not self.page.duplicate:
            return
        self.loading = True
        route = Route(self.page)
        self.page.routes.append(route)
        self.page.ids.append(str(len(self.page.ids)))
        self.page.handler(route)


class Expect:
    def __init__(self, button):
        self.button = button

    def to_have_class(self, pattern):
        actual = 'ant-btn ant-btn-loading' if self.button.loading else 'ant-btn'
        if not pattern.search(actual):
            raise AssertionError('loading not observable')


class BarrierTests(unittest.TestCase):
    def run_helper(self, page, report):
        with patch('template_control_checks.expect', Expect):
            return submit_real(page, page, page.ids, page.button, BODY, 400, report)

    def test_real_response_is_observable_only_after_continue_once(self):
        page, report = Page(), {}
        self.assertEqual(self.run_helper(page, report).status, 400)
        self.assertEqual(page.delivered, 1)
        self.assertTrue(page.removed)
        self.assertEqual([route.attempts for route in page.routes], [1])
        self.assertEqual(report['real_imports'][0]['status'], 400)

    def test_duplicate_submission_fails_and_releases_every_owned_route(self):
        page, report = Page(duplicate=True), {}
        with self.assertRaisesRegex(AssertionError, 'duplicate import'):
            self.run_helper(page, report)
        self.assertTrue(page.removed)
        self.assertTrue(all(route.released for route in page.routes))
        self.assertEqual(page.delivered, 2)
        self.assertIn('duplicate import', report['failure'])

    def test_cleanup_release_exception_preserves_first_assertion(self):
        page, report = Page(duplicate=True, release_fails=True), {}
        with self.assertRaisesRegex(AssertionError, 'duplicate import'):
            self.run_helper(page, report)
        self.assertTrue(page.removed)
        self.assertIn('duplicate import', report['failure'])
        self.assertEqual(len(report['cleanup_errors']), 2)
        self.assertTrue(all(route.attempts == 1 for route in page.routes))

    def test_release_failure_never_becomes_a_response_pass(self):
        page, report = Page(release_fails=True), {}
        with self.assertRaisesRegex(TimeoutError, 'release failed'):
            self.run_helper(page, report)
        self.assertTrue(page.removed)
        self.assertEqual(page.delivered, 0)
        self.assertEqual(page.routes[0].attempts, 2)
        self.assertNotIn('real_imports', report)

    def test_wrong_response_status_is_not_fabricated_to_expected(self):
        page, report = Page(status=200), {}
        with self.assertRaisesRegex(AssertionError, 'expected 400'):
            self.run_helper(page, report)
        self.assertTrue(page.removed)
        self.assertEqual(page.delivered, 1)

    def test_changed_wire_body_is_rejected_after_release(self):
        page, report = Page(body='stale saved YAML'), {}
        with self.assertRaisesRegex(AssertionError, 'submitted YAML changed'):
            self.run_helper(page, report)
        self.assertTrue(page.removed)
        self.assertNotIn('real_imports', report)


if __name__ == '__main__':
    unittest.main()
