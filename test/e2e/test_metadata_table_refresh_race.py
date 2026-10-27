"""Captured en3 navigation race replay at the real refresh() seam.

No browser or management mock. Replays old/current Request identities, shared
frame/URL/status and document commits, including old requests responding late.
"""
import copy
import unittest
from unittest.mock import patch

from metadata_table_checks import refresh

OLD_RESOURCE_GONE = 'Protocol error (Network.getResponseBody): No resource with given identifier found'


class RecordedFrame:
    url = 'http://owned/images'


class RecordedRequest:
    resource_type = "fetch"
    def is_navigation_request(self):
        return False

    def __init__(self, page, document, query=''):
        self.document = document
        self.url = 'http://owned/api/v1/images' + query
        self.method, self.post_data, self.frame = 'GET', None, page.main_frame


class DocumentRequest:
    method, resource_type = 'GET', 'document'
    def __init__(self, page, url='http://owned/images'):
        self.frame, self.url = page.main_frame, url
    def is_navigation_request(self):
        return True


class RecordedResponse:
    def __init__(self, page, document, body, query=''):
        self.page, self.document, self.payload = page, document, copy.deepcopy(body)
        self.request = RecordedRequest(page, document, query)
        self.url, self.status = self.request.url, 200

    def json(self):
        self.page.body_reads.append(self.document)
        if self.document != self.page.document:
            raise RuntimeError(OLD_RESOURCE_GONE)
        if self.page.current_body_error:
            raise ValueError(self.page.current_body_error)
        return copy.deepcopy(self.payload)


class ResponseWait:
    def __init__(self, page, predicate):
        self.page, self.predicate, self.value = page, predicate, None

    def __enter__(self):
        self.page.waiter = self
        if self.page.response_phase == 'after-heading':
            self.page.respond(self.page.new)
        return self

    def __exit__(self, typ, value, tb):
        self.page.waiter = None
        if typ is None and self.value is None:
            raise AssertionError('replay waiter never matched a response')
        return False


class RecordedPage:
    def __init__(self, overlaps=True, *, late_old=False, old_before_subscription=False,
                 query='', current_body_error='', remove_error='', response_phase='navigation', heading_error=''):
        self.document, self.main_frame, self.waiter = 'old', RecordedFrame(), None
        self.body_reads, self.events, self.listeners = [], [], {}
        self.overlaps, self.late_old = overlaps, late_old
        self.old_before_subscription = old_before_subscription
        self.current_body_error, self.remove_error = current_body_error, remove_error
        self.goto_url = None
        self.response_phase, self.heading_error = response_phase, heading_error
        self.old = RecordedResponse(self, 'old', {'items': [{'id': 1, 'name': 'old'}], 'total': 1}, query)
        self.new = RecordedResponse(self, 'new', {'items': [{'id': 2, 'name': 'new'}], 'total': 1}, query)

    def on(self, event, callback):
        self.listeners.setdefault(event, []).append(callback)

    def remove_listener(self, event, callback):
        self.listeners[event].remove(callback)
        if event == self.remove_error:
            raise RuntimeError('remove-listener-' + event + '-failed')

    def emit(self, event, value):
        for callback in list(self.listeners.get(event, [])):
            callback(value)

    def expect_response(self, predicate):
        self.events.append('waiter-registered-before-navigation')
        return ResponseWait(self, predicate)

    def respond(self, response):
        self.events.append(response.document + '-response')
        self.emit('response', response)
        if self.waiter is not None and self.waiter.value is None and self.waiter.predicate(response):
            self.waiter.value = response

    def navigate(self):
        self.events.append('navigation-start')
        if self.overlaps:
            if not self.old_before_subscription:
                self.emit('request', self.old.request)
            if not self.late_old:
                self.respond(self.old)
        self.emit('request', DocumentRequest(self))
        self.document = self.new.document
        self.events.append('new-main-document-commit')
        self.emit('framenavigated', self.main_frame)
        if self.overlaps and self.late_old:
            self.respond(self.old)
        self.emit('request', self.new.request)
        if self.response_phase == 'navigation':
            self.respond(self.new)
        self.events.append('navigation-return')

    def get_by_role(self, role, *, name, exact):
        assert (role, name, exact) == ('heading', 'Images', True)
        return self

    def to_be_visible(self):
        self.events.append('heading-wait')
        if self.response_phase == 'heading':
            self.respond(self.new)
        if self.heading_error:
            raise RuntimeError(self.heading_error)
        self.events.append('heading-visible')

    def reload(self):
        self.events.append('reload')
        self.navigate()

    def goto(self, url):
        self.goto_url = url
        self.events.append('goto')
        self.navigate()


class PatchedExpectation(unittest.TestCase):
    def setUp(self):
        self.expectation = patch('metadata_table_checks.expect', lambda locator: locator)
        self.expectation.start()
        self.addCleanup(self.expectation.stop)


class RefreshDocumentRace(PatchedExpectation):
    def test_old_request_and_response_before_commit_are_rejected(self):
        page = RecordedPage()
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}), [{'id': 2, 'name': 'new'}])
        self.assertEqual(page.body_reads, ['new'])

    def test_old_request_created_early_response_after_new_commit_is_still_rejected(self):
        page = RecordedPage(late_old=True)
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}), [{'id': 2, 'name': 'new'}])
        self.assertEqual(page.body_reads, ['new'])
        self.assertEqual((page.old.url, page.old.request.method, page.old.status),
                         (page.new.url, page.new.request.method, page.new.status))
        self.assertIs(page.old.request.frame, page.new.request.frame)

    def test_old_request_unseen_before_subscription_cannot_match_a_late_response(self):
        page = RecordedPage(late_old=True, old_before_subscription=True)
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}), [{'id': 2, 'name': 'new'}])
        self.assertEqual(page.body_reads, ['new'])

    def test_single_current_document_response_control(self):
        page = RecordedPage(overlaps=False)
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}), [{'id': 2, 'name': 'new'}])
        self.assertEqual(page.body_reads, ['new'])


class RefreshListenerOwnership(PatchedExpectation):
    def test_success_unregisters_both_listeners_and_records_selected_request_epoch(self):
        page=RecordedPage(late_old=True)
        ledger={}
        refresh(page,'http://owned', {'page': {'title': 'Images'}},ledger=ledger)
        observation=ledger['refreshes'][0]
        self.assertEqual(observation['document_commits'],[{'epoch':1,'url':'http://owned/images'}])
        self.assertEqual([r['created_after_commit'] for r in observation['requests']],[False,True])
        self.assertEqual(observation['selected_request_sequence'],2)
        self.assertEqual(page.listeners,{'framenavigated':[],'request':[],'response':[]})

    def test_foreign_frame_commit_cannot_authorize_main_frame_api_request(self):
        class ForeignCommitPage(RecordedPage):
            def navigate(self):
                self.document='new'
                self.emit('framenavigated',RecordedFrame())
                self.emit('request',self.new.request)
                self.respond(self.new)
        page=ForeignCommitPage(overlaps=False)
        with self.assertRaisesRegex(AssertionError,'never matched'):
            refresh(page,'http://owned', {'page': {'title': 'Images'}})
        self.assertEqual(page.body_reads,[])
        self.assertEqual(page.listeners,{'framenavigated':[],'request':[],'response':[]})

    def test_primary_body_error_is_not_hidden_by_secondary_listener_removal_failure(self):
        page=RecordedPage(overlaps=False,current_body_error='broken current JSON',remove_error='request')
        ledger={}
        with self.assertRaisesRegex(ValueError,'broken current JSON'):
            refresh(page,'http://owned', {'page': {'title': 'Images'}},ledger=ledger)
        self.assertEqual(page.body_reads,['new'])
        self.assertEqual(page.listeners,{'framenavigated':[],'request':[],'response':[]})
        self.assertIn('remove-listener-request-failed',ledger['refreshes'][0]['cleanup_errors'][0])

    def test_listener_removal_error_is_not_a_success_when_business_passes(self):
        page=RecordedPage(overlaps=False,remove_error='request')
        with self.assertRaisesRegex(AssertionError,'listener cleanup failed'):
            refresh(page,'http://owned', {'page': {'title': 'Images'}})
        self.assertEqual(page.listeners,{'framenavigated':[],'request':[],'response':[]})


class RefreshReadinessPhases(PatchedExpectation):
    def test_same_document_replace_state_keeps_early_response_generation(self):
        class ReplaceStatePage(RecordedPage):
            def navigate(self):
                super().navigate()
                self.emit('framenavigated', self.main_frame)
        page = ReplaceStatePage(overlaps=False)
        ledger = {}
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}, ledger=ledger), [{'id': 2, 'name': 'new'}])
        self.assertEqual(len(ledger['refreshes'][0]['document_commits']), 1)

    def test_redirect_navigation_requests_authorize_one_actual_commit(self):
        class RedirectPage(RecordedPage):
            def navigate(self):
                self.emit('request', DocumentRequest(self, 'http://owned/login'))
                super().navigate()
        page = RedirectPage(overlaps=False)
        ledger = {}
        refresh(page, 'http://owned', {'page': {'title': 'Images'}}, ledger=ledger)
        self.assertEqual(len(ledger['refreshes'][0]['document_commits']), 1)

    def test_two_real_reloads_reject_previous_document_late_response(self):
        page = RecordedPage(overlaps=False)
        refresh(page, 'http://owned', {'page': {'title': 'Images'}})
        page.old, page.new = page.new, RecordedResponse(page, 'third', {'items': [{'id': 3, 'name': 'third'}], 'total': 1})
        page.overlaps, page.late_old = True, True
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}), [{'id': 3, 'name': 'third'}])
        self.assertEqual(page.body_reads, ['new', 'third'])

    def test_early_response_before_heading_is_retained_without_second_poll(self):
        page = RecordedPage(overlaps=False)
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}), [{'id': 2, 'name': 'new'}])
        self.assertNotIn('waiter-registered-before-navigation', page.events)
        self.assertLess(page.events.index('new-response'), page.events.index('heading-visible'))

    def test_response_during_heading_wait_is_retained(self):
        page = RecordedPage(overlaps=False, response_phase='heading')
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}), [{'id': 2, 'name': 'new'}])
        self.assertNotIn('waiter-registered-before-navigation', page.events)

    def test_response_at_waiter_registration_after_heading_is_not_lost(self):
        page = RecordedPage(overlaps=False, response_phase='after-heading')
        self.assertEqual(refresh(page, 'http://owned', {'page': {'title': 'Images'}}), [{'id': 2, 'name': 'new'}])
        self.assertLess(page.events.index('heading-visible'), page.events.index('new-response'))

    def test_heading_failure_preserves_primary_and_cleans_every_observer(self):
        page = RecordedPage(overlaps=False, heading_error='real heading missing', remove_error='request')
        with self.assertRaisesRegex(RuntimeError, 'real heading missing'):
            refresh(page, 'http://owned', {'page': {'title': 'Images'}})
        self.assertEqual(page.body_reads, [])
        self.assertEqual(page.listeners, {'framenavigated': [], 'request': [], 'response': []})


if __name__ == '__main__':
    unittest.main()
