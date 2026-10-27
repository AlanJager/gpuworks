"""Offline CONNECT/TLS RunPod boundary tests; no product/Chrome/cloud calls.

Run: PYTHONPATH=test/e2e python3 -m unittest test_provider_default_endpoint_fixture -v
"""
from http.client import HTTPConnection, HTTPSConnection, HTTPResponse
import json
from pathlib import Path
import socket
import ssl
import tempfile
import unittest
from unittest import mock

from provider_default_endpoint_fixture import (
    AUTHORITY, HOST, ProviderDefaultEndpointFixture, certificates)
from test_provider_endpoint_fixture import KEY_A, KEY_B

TOKEN = 'fixture-only-api-key'


class DefaultEndpointFixtureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.directory.cleanup)
        cls.certificates = certificates(Path(cls.directory.name) / 'default')

    def setUp(self):
        # Every connection in these tests must remain on the numeric loopback
        # proxy address. Even an accidental target fallback cannot reach DNS.
        actual_lookup = socket.getaddrinfo

        def lookup(host, *args, **kwargs):
            if host != '127.0.0.1':
                raise AssertionError('fixture attempted non-loopback DNS')
            return actual_lookup(host, *args, **kwargs)

        self.lookup = mock.patch('socket.getaddrinfo', side_effect=lookup)
        self.lookup.start()
        self.addCleanup(self.lookup.stop)
        self.fixture = ProviderDefaultEndpointFixture(self.directory.name, TOKEN, keys=[KEY_A],
                                                      certificate_files=self.certificates)
        self.fixture.__enter__()
        self.addCleanup(self.fixture.stop)

    def tearDown(self):
        self.assertTrue(self.fixture.stop())
        self.assertEqual(self.fixture.active_handlers, 0)
        self.assertEqual(self.fixture.active_sockets, 0)
        self.assertFalse(self.fixture.thread.is_alive())
        self.assertEqual(self.fixture.server.socket.fileno(), -1)
        self.assertEqual(self.fixture.protocol.server.socket.fileno(), -1)
        with socket.socket() as probe:
            self.assertNotEqual(probe.connect_ex(self.fixture.address), 0)

    def context(self, fixture=None):
        result = ssl.create_default_context(cafile=str((fixture or self.fixture).ca))
        result.set_alpn_protocols(['h2', 'http/1.1'])
        return result

    def request(self, path='/v2/pods', method='GET', body=None, *, raw=None,
                authorization='Bearer ' + TOKEN, host=HOST, context=None, fixture=None):
        fixture = fixture or self.fixture
        connection = HTTPSConnection(*fixture.address, context=context or self.context(fixture), timeout=3)
        connection.set_tunnel(HOST, 443)
        headers = {'Host': host, 'Accept': 'application/json'}
        if authorization is not None:
            headers['Authorization'] = authorization
        if body is not None or raw is not None:
            headers['Content-Type'] = 'application/json'
        try:
            connection.request(method, path, body=raw if raw is not None else json.dumps(body) if body is not None else None,
                               headers=headers)
            self.last_alpn = connection.sock.selected_alpn_protocol()
            response = connection.getresponse()
            return response.status, response.headers.get_content_type(), json.loads(response.read())
        finally:
            connection.close()

    def tunnel(self):
        connection = socket.create_connection(self.fixture.address, timeout=3)
        self.addCleanup(connection.close)
        connection.sendall(('CONNECT ' + AUTHORITY + ' HTTP/1.1\r\nHost: ' + AUTHORITY + '\r\n\r\n').encode())
        # Read exactly the header so TLS bytes never sit in a buffered reader.
        header = bytearray()
        while not header.endswith(b'\r\n\r\n'):
            part = connection.recv(1)
            self.assertTrue(part, 'proxy closed before CONNECT response')
            header.extend(part)
        self.assertTrue(header.startswith(b'HTTP/1.0 200'))
        return connection

    def test_default_authority_sni_alpn_and_exact_pods_contract(self):
        status, content_type, body = self.request()
        self.assertEqual((status, content_type), (200, 'application/json'))
        self.assertEqual(body, {'pods': [], 'pagination': {'nextCursor': None, 'hasNextPage': False}})
        self.assertEqual(self.last_alpn, 'http/1.1')
        self.assertIn({'event': 'connect', 'authority': AUTHORITY, 'accepted': True}, self.fixture.events)
        self.assertIn({'event': 'sni', 'hostname': HOST, 'accepted': True}, self.fixture.events)
        self.assertEqual([(r['method'], r['path'], r['authorized'], r['status']) for r in self.fixture.requests],
                         [('GET', '/v2/pods', True, 200)])

    def test_wrong_connect_targets_and_plain_proxy_methods_fail_closed(self):
        for authority in ('other.invalid:443', HOST + ':444', '127.0.0.1:443'):
            with self.subTest(authority=authority):
                connection = HTTPConnection(*self.fixture.address, timeout=3)
                try:
                    connection.request('CONNECT', authority, headers={'Host': authority})
                    response = connection.getresponse()
                    self.assertEqual(response.status, 403)
                    response.read()
                finally:
                    connection.close()
        connection = HTTPConnection(*self.fixture.address, timeout=3)
        try:
            connection.request('GET', 'https://' + HOST + '/v2/pods')
            response = connection.getresponse()
            self.assertEqual(response.status, 405)
            response.read()
        finally:
            connection.close()
        self.assertEqual(self.fixture.requests, [])

    def test_untrusted_ca_fails_without_http_or_fallback(self):
        with self.assertRaises(ssl.SSLCertVerificationError):
            self.request(context=ssl.create_default_context())
        self.assertEqual(self.fixture.requests, [])

    def test_connect_ambiguous_host_and_body_framing_are_rejected(self):
        for extra in ('Host: other.invalid', 'Content-Length: 1',
                      'Content-Length: 0\r\nContent-Length: 0', 'Transfer-Encoding: chunked'):
            with self.subTest(extra=extra), socket.create_connection(self.fixture.address, timeout=3) as connection:
                connection.sendall(('CONNECT ' + AUTHORITY + ' HTTP/1.1\r\nHost: ' + AUTHORITY
                                    + '\r\n' + extra + '\r\n\r\n').encode())
                response = HTTPResponse(connection)
                response.begin()
                self.assertEqual(response.status, 403)
                response.read()
                response.close()
        self.assertEqual(self.fixture.requests, [])

    def test_wrong_sni_is_rejected_even_without_client_hostname_checks(self):
        context = self.context()
        context.check_hostname = False
        with self.assertRaises(ssl.SSLError):
            context.wrap_socket(self.tunnel(), server_hostname='other.invalid')
        self.assertIn({'event': 'sni', 'hostname': 'other.invalid', 'accepted': False}, self.fixture.events)
        self.assertEqual(self.fixture.requests, [])

    def test_trusted_ca_wrong_leaf_hostname_is_still_rejected(self):
        self.fixture.stop()
        files = certificates(Path(self.directory.name) / 'wrong-hostname', hostname='wrong.invalid')
        with ProviderDefaultEndpointFixture(self.directory.name, TOKEN, certificate_files=files) as fixture:
            with self.assertRaisesRegex(ssl.SSLCertVerificationError, 'Hostname mismatch'):
                self.request(fixture=fixture)
            self.assertEqual(fixture.requests, [])
        self.assertEqual(fixture.active_handlers, 0)
        self.assertEqual(fixture.active_sockets, 0)

    def test_missing_malformed_invalid_and_forbidden_credentials(self):
        for authorization in (None, TOKEN, 'Bearer wrong-key'):
            for method in ('GET', 'PUT'):
                status, content_type, body = self.request('/v2/account/ssh-keys', method,
                    {'keys': [KEY_B]} if method == 'PUT' else None, authorization=authorization)
                self.assertEqual((status, content_type, body['status']), (401, 'application/problem+json', 401))
        self.fixture.protocol.forbidden = True
        self.assertEqual(self.request('/v2/account/ssh-keys', 'PUT', {'keys': [KEY_B]})[0], 403)
        self.fixture.protocol.forbidden = False
        self.fixture.protocol.reject = True
        self.assertEqual(self.request()[0], 401)
        self.fixture.protocol.reject = False
        self.assertEqual(self.request()[0], 200)
        self.assertEqual(self.fixture.protocol.keys, [KEY_A])

    def test_invalid_and_ambiguous_request_framing_fails_before_protocol(self):
        for framing in ('Content-Length: -1', 'Content-Length: +1', 'Content-Length: 1_0',
                        'Content-Length: not-a-number',
                        'Content-Length: 0\r\nContent-Length: 1', 'Transfer-Encoding: chunked'):
            with self.subTest(framing=framing):
                encrypted = self.context().wrap_socket(self.tunnel(), server_hostname=HOST)
                try:
                    encrypted.sendall(('PUT /v2/account/ssh-keys HTTP/1.1\r\nHost: ' + HOST + '\r\n'
                                       + framing + '\r\n\r\n').encode())
                    response = HTTPResponse(encrypted)
                    response.begin()
                    self.assertEqual(response.status, 400)
                    response.read()
                    response.close()
                finally:
                    encrypted.close()
        self.assertEqual(self.fixture.requests, [])
        self.assertEqual(self.fixture.protocol.keys, [KEY_A])

    def test_duplicate_host_is_rejected_before_protocol(self):
        encrypted = self.context().wrap_socket(self.tunnel(), server_hostname=HOST)
        try:
            encrypted.sendall(('GET /v2/pods HTTP/1.1\r\nHost: ' + HOST
                               + '\r\nHost: other.invalid\r\n\r\n').encode())
            response = HTTPResponse(encrypted)
            response.begin()
            self.assertEqual(response.status, 421)
            response.read()
            response.close()
        finally:
            encrypted.close()
        self.assertEqual(self.fixture.requests, [])

    def test_full_account_key_replacement_can_preserve_and_remove_existing_keys(self):
        self.assertEqual(self.request('/v2/account/ssh-keys')[2], {'keys': [KEY_A]})
        for keys in ([KEY_A, KEY_B], [KEY_B], []):
            self.assertEqual(self.request('/v2/account/ssh-keys', 'PUT', {'keys': keys}),
                             (200, 'application/json', {'keys': keys}))
            self.assertEqual(self.request('/v2/account/ssh-keys')[2], {'keys': keys})

    def test_bad_json_types_and_unknown_fields_preserve_keys(self):
        for body in ({}, [], {'keys': None}, {'keys': 'key'}, {'keys': [False]}, {'keys': [42]},
                     {'keys': ['not-a-key']}, {'keys': ['ssh-ed25519 abc\nmalformed']}, {'keys': [], 'extra': True}):
            with self.subTest(body=body):
                status, content_type, response = self.request('/v2/account/ssh-keys', 'PUT', body)
                self.assertEqual((status, content_type, response['status']), (422, 'application/problem+json', 422))
        self.assertEqual(self.request('/v2/account/ssh-keys', 'PUT', raw=b'{')[0], 400)
        self.assertEqual(self.request('/v2/account/ssh-keys')[2], {'keys': [KEY_A]})

    def test_unknown_requests_and_host_mismatch_are_refused(self):
        for path in ('/v2/pods?unknown=1', '/v2/unknown', '/proxy-a/v2/pods'):
            self.assertEqual(self.request(path)[0], 404)
        for method in ('POST', 'DELETE', 'PATCH', 'OPTIONS'):
            self.assertEqual(self.request('/v2/account/ssh-keys', method)[0], 405)
        self.assertEqual(self.request(host='other.invalid')[0], 421)
        self.assertEqual(self.fixture.protocol.keys, [KEY_A])

    def test_disconnect_during_tls_and_close_of_idle_tunnel_release_every_socket(self):
        raw = self.tunnel()
        raw.close()
        encrypted = self.context().wrap_socket(self.tunnel(), server_hostname=HOST)
        self.addCleanup(encrypted.close)
        self.assertTrue(self.fixture.stop())
        self.assertEqual(self.fixture.active_sockets, 0)
        self.assertEqual(self.fixture.active_handlers, 0)

    def test_stop_interrupts_handshake_and_incomplete_request_body(self):
        raw = self.tunnel()  # Leave one handler waiting for a ClientHello.
        encrypted = self.context().wrap_socket(self.tunnel(), server_hostname=HOST)
        self.addCleanup(encrypted.close)
        encrypted.sendall(('PUT /v2/account/ssh-keys HTTP/1.1\r\nHost: ' + HOST + '\r\n'
                           'Authorization: Bearer ' + TOKEN + '\r\nContent-Length: 100\r\n\r\n{').encode())
        self.assertTrue(self.fixture.stop())
        raw.close()
        self.assertEqual(self.fixture.protocol.keys, [KEY_A])

    def test_close_failure_is_not_reported_as_clean_and_can_be_retried(self):
        with mock.patch.object(self.fixture.server, 'server_close', side_effect=OSError('injected close failure')):
            with self.assertRaisesRegex(OSError, 'injected close failure'):
                self.fixture.stop()
        self.assertTrue(self.fixture.stop())

    def test_environment_is_per_child_and_does_not_change_os_environment(self):
        import os
        before = dict(os.environ)
        environment = self.fixture.child_environment()
        self.assertEqual(environment['HTTPS_PROXY'], self.fixture.proxy_url)
        self.assertEqual(environment['NO_PROXY'], '127.0.0.1,localhost,::1')
        self.assertEqual(environment['SSL_CERT_FILE'], str(self.fixture.ca))
        self.assertEqual(list(Path(environment['SSL_CERT_DIR']).iterdir()), [])
        self.assertIsNone(environment['ALL_PROXY'])
        self.assertTrue(dict(os.environ) == before, 'fixture changed parent environment')
        for path in self.certificates:
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)


if __name__ == '__main__':
    unittest.main()
