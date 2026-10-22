"""Real HTTP negative/positive protocol and handler-lifecycle checks."""
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler
from unittest.mock import patch
import json
import socket
import struct
import threading
import unittest
from urllib.parse import urlsplit

from model_download_fixture import ModelDownload, REVISION


class ModelDownloadTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModelDownload('unit-test').start()
        self.addCleanup(self.assert_stopped)

    def assert_stopped(self):
        result = self.fixture.stop()
        self.assertEqual(result, {'handlers': 0, 'thread_stopped': True,
                                  'listener_stopped': True, 'stopped': True})

    def request(self, path=None, method='GET', headers=None, body=None):
        host = urlsplit(self.fixture.base)
        connection = http.client.HTTPConnection(host.hostname, host.port, timeout=5)
        connection.request(method, path or self.fixture.root + '/', body, headers or {})
        response = connection.getresponse()
        result = response.status, dict(response.headers), response.read()
        connection.close()
        return result

    def test_negative_unknown_method_query_credentials_and_ranges(self):
        for path, method, headers, status in (
            ('/unowned/config.json', 'GET', {}, 404),
            (self.fixture.root + '/config.json?wrong=1', 'GET', {}, 404),
            (self.fixture.root + '/config.json', 'POST', {}, 405),
            (self.fixture.root + '/config.json', 'GET', {'Authorization': 'Bearer wrong'}, 400),
            (self.fixture.root + '/model.safetensors', 'GET', {'Range': 'bytes=1-0'}, 416),
            (self.fixture.root + '/model.safetensors', 'GET', {'Range': 'bytes=0-1,4-5'}, 416),
            (self.fixture.root + '/model.safetensors', 'GET', {'Range': 'bytes=999999999-'}, 416),
        ):
            with self.subTest(path=path, method=method, headers=headers):
                self.assertEqual(self.request(path, method, headers)[0], status)
        self.assertEqual(self.request(self.fixture.root + '/config.json', body=b'wrong')[0], 400)
        with self.assertRaises(ValueError):
            self.fixture.set_weight_failure(1)
        with self.assertRaises(ValueError):
            self.fixture.interrupt_next_weight(True)
        with self.assertRaises(ValueError):
            ModelDownload('../unowned')

    def test_weight_503_does_not_break_metadata_and_recovers(self):
        self.fixture.set_weight_failure(True)
        root = self.fixture.root + '/'
        self.assertEqual(self.request(root)[0], 200)
        self.assertEqual(self.request(root + 'config.json')[2], self.fixture.files['config.json'])
        for method in ('GET', 'HEAD'):
            self.assertEqual(self.request(root + 'model.safetensors', method)[0], 503)
        self.fixture.set_weight_failure(False)
        self.assertEqual(self.request(root + 'model.safetensors')[2], self.fixture.files['model.safetensors'])

    def test_exact_bytes_head_range_and_safetensors(self):
        for name, payload in self.fixture.files.items():
            path = self.fixture.root + '/' + name
            status, headers, body = self.request(path)
            self.assertEqual((status, int(headers['Content-Length']), body), (200, len(payload), payload))
            status, headers, body = self.request(path, 'HEAD')
            self.assertEqual((status, int(headers['Content-Length']), body), (200, len(payload), b''))
            status, headers, body = self.request(path, headers={'Range': 'bytes=7-18'})
            self.assertEqual((status, headers['Content-Range'], body), (206, f'bytes 7-18/{len(payload)}', payload[7:19]))
        weight = self.fixture.files['model.safetensors']
        size = struct.unpack('<Q', weight[:8])[0]
        tensor = json.loads(weight[8:8+size])['owned_fixture']
        self.assertEqual(tensor, {'dtype': 'U8', 'shape': [2097152], 'data_offsets': [0, 2097152]})
        self.assertEqual(weight[8+size:], bytes(range(256)) * 8192)
        self.assertTrue(self.fixture.wait_idle())
        self.assertTrue(all(r['complete'] for r in self.fixture.audit()))
        heads = [r for r in self.fixture.audit() if r['method'] == 'HEAD']
        self.assertTrue(all(r['bytes_sent'] == 0 and r['sha256_sent'] == hashlib.sha256(b'').hexdigest() for r in heads))

    def test_partial_disconnect_observed_prefix_then_range_recovery(self):
        weight = self.fixture.files['model.safetensors']
        self.fixture.interrupt_next_weight(after_bytes=8192, hold=True)
        host = urlsplit(self.fixture.base)
        connection = http.client.HTTPConnection(host.hostname, host.port, timeout=5)
        connection.request('GET', self.fixture.root + '/model.safetensors')
        response = connection.getresponse()
        self.assertTrue(self.fixture.wait_interrupted())
        partial = response.read(8192)
        self.assertEqual(partial, weight[:8192])
        interim = self.fixture.audit()[-1]
        self.assertEqual((interim['bytes_sent'], interim['planned_bytes'], interim['complete']), (8192, len(weight), False))
        self.fixture.release()
        with self.assertRaises(http.client.IncompleteRead):
            response.read()
        connection.close()
        status, headers, suffix = self.request(self.fixture.root + '/model.safetensors', headers={'Range': 'bytes=8192-'})
        self.assertEqual((status, headers['Content-Range']), (206, f'bytes 8192-{len(weight)-1}/{len(weight)}'))
        self.assertEqual(hashlib.sha256(partial + suffix).digest(), hashlib.sha256(weight).digest())
        record = next(r for r in self.fixture.audit() if r['interrupted'])
        self.assertEqual((record['bytes_sent'], record['sha256_sent']), (8192, hashlib.sha256(weight[:8192]).hexdigest()))

    def test_shutdown_releases_held_handler_and_closes_socket(self):
        self.fixture.interrupt_next_weight(8192)
        host = urlsplit(self.fixture.base)
        connection = http.client.HTTPConnection(host.hostname, host.port, timeout=5)
        connection.request('GET', self.fixture.root + '/model.safetensors')
        response = connection.getresponse()
        self.assertTrue(self.fixture.wait_interrupted())
        self.assertEqual(response.read(8192), self.fixture.files['model.safetensors'][:8192])
        self.assertTrue(self.fixture.stop()['stopped'])
        with self.assertRaises(http.client.IncompleteRead):
            response.read()
        connection.close()
        with self.assertRaises(OSError):
            socket.create_connection((host.hostname, host.port), timeout=.2)

    def test_stop_reports_handler_residual_without_claiming_success(self):
        blocked = threading.Event()
        class Stuck:
            def shutdown(self, _):
                blocked.set()
        stuck = Stuck()
        with self.fixture._cv:
            self.fixture._sockets.add(stuck)
        result = self.fixture.stop(timeout=.1)
        self.assertTrue(blocked.is_set())
        self.assertFalse(result['stopped'])
        self.assertEqual(result['handlers'], 1)
        with self.fixture._cv:
            self.fixture._sockets.remove(stuck)

    def test_origin_and_idle_connection_are_fail_closed(self):
        path = self.fixture.root + '/config.json'
        self.assertEqual(self.request(path, headers={'Origin': 'https://foreign.invalid'})[0], 403)
        status, headers, body = self.request(path, headers={'Origin': 'http://127.0.0.1:8800'})
        self.assertEqual((status, headers['Access-Control-Allow-Origin'], body),
                         (200, 'http://127.0.0.1:8800', self.fixture.files['config.json']))
        host = urlsplit(self.fixture.base)
        idle = socket.create_connection((host.hostname, host.port), timeout=3)
        # Send an incomplete request, so shutdown must close a parsing handler too.
        idle.sendall(b'GET / HTTP/1.1\r\n')
        with self.fixture._cv:
            self.assertTrue(self.fixture._cv.wait_for(lambda: bool(self.fixture._sockets), timeout=3))
        self.assertTrue(self.fixture.stop()['stopped'])
        self.assertEqual(idle.recv(1), b'')
        idle.close()

    def test_accepted_socket_before_handler_setup_cannot_report_stopped(self):
        entered, release = threading.Event(), threading.Event()
        original = BaseHTTPRequestHandler.setup
        def gated_setup(handler):
            entered.set()
            if not release.wait(5):
                raise RuntimeError('test setup barrier timed out')
            original(handler)
        host = urlsplit(self.fixture.base)
        connection = http.client.HTTPConnection(host.hostname, host.port, timeout=3)
        try:
            with patch.object(BaseHTTPRequestHandler, 'setup', gated_setup):
                connection.request('GET', self.fixture.root + '/config.json')
                self.assertTrue(entered.wait(3), 'request never reached setup gate')
                observed = self.fixture.stop(timeout=.2)
                self.assertFalse(observed['stopped'], 'pending accepted handler falsely reported reclaimed')
                self.assertEqual(observed['handlers'], 1)
                release.set()
                with self.assertRaises((OSError, http.client.HTTPException)):
                    connection.getresponse()
        finally:
            release.set()
            connection.close()
        self.assertTrue(self.fixture.wait_idle())

    def test_hub_wire_lists_and_resolve_deliver_same_exact_files(self):
        for prefix in ('/hf/api/models/', '/ms/api/v1/models/'):
            path = prefix + self.fixture.repo
            listing = path + ('/tree/main?recursive=true' if prefix.startswith('/hf') else '/repo/files?Recursive=true&Revision=master')
            status, _, raw = self.request(listing)
            self.assertEqual(status, 200)
            doc = json.loads(raw)
            files = doc if isinstance(doc, list) else doc['Data']['Files']
            projected = {f.get('path', f.get('Path')): f.get('size', f.get('Size')) for f in files}
            self.assertEqual(projected, {n: len(b) for n, b in self.fixture.files.items()})
        for prefix in ('/hf/' + self.fixture.repo + '/resolve/main/', '/ms/models/' + self.fixture.repo + '/resolve/' + REVISION + '/'):
            for name, value in self.fixture.files.items():
                self.assertEqual(self.request(prefix + name)[2], value)
        self.assertEqual(self.request('/ms/api/v1/models/' + self.fixture.repo + '/repo?FilePath=config.json')[2], self.fixture.files['config.json'])
        self.assertEqual(self.request('/hf/api/models/' + self.fixture.repo + '/tree/main?recursive=1')[0], 400)
        self.assertEqual(self.request('/ms/api/v1/models/' + self.fixture.repo + '/repo/files?Recursive=false')[0], 400)


if __name__ == '__main__':
    unittest.main()
