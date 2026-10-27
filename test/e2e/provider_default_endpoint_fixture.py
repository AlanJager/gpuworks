"""Strict offline CONNECT/TLS boundary for RunPod's unchanged default URL.

Only api.runpod.io:443 is accepted. No connection is forwarded or resolved.
RunPod HTTP/auth/key semantics reuse provider_endpoint_fixture.RunPodFixture.
Certificates use the repository's openssl subprocess pattern; no host trust
store or process environment is modified. Files belong in run private/.
"""
from __future__ import annotations

import copy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
from pathlib import Path
import socket
import ssl
import subprocess
import threading

from provider_endpoint_fixture import RunPodFixture

HOST = 'api.runpod.io'
AUTHORITY = HOST + ':443'


def certificates(directory, hostname=HOST):
    """Create a private CA and a SAN-bearing server leaf, never global trust."""
    directory = Path(directory).resolve()
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    directory.chmod(0o700)
    ca, ca_key = directory / 'ca.pem', directory / 'ca-key.pem'
    cert, key, csr = directory / 'server.pem', directory / 'server-key.pem', directory / 'server.csr'
    extension = directory / 'server.ext'
    if any(path.exists() for path in (ca, ca_key, cert, key, csr, extension)):
        raise FileExistsError('fixture certificate directory already contains certificate files')
    # hostname is passed as data, never shell code. The extension cannot gain
    # another line or SAN from a caller-supplied value.
    if not hostname or any(char not in 'abcdefghijklmnopqrstuvwxyz0123456789.-' for char in hostname):
        raise ValueError('fixture certificate hostname must be one DNS name')
    extension.write_text('basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n'
                         'extendedKeyUsage=serverAuth\nsubjectAltName=DNS:' + hostname + '\n')
    commands = (
        ['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-sha256',
         '-keyout', str(ca_key), '-out', str(ca), '-subj', '/CN=RunPod offline fixture CA',
         '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign'],
        ['openssl', 'req', '-new', '-newkey', 'rsa:2048', '-nodes', '-sha256',
         '-keyout', str(key), '-out', str(csr), '-subj', '/CN=' + hostname],
        ['openssl', 'x509', '-req', '-in', str(csr), '-CA', str(ca), '-CAkey', str(ca_key),
         '-CAcreateserial', '-out', str(cert), '-days', '1', '-sha256', '-extfile', str(extension)],
    )
    try:
        for command in commands:
            subprocess.run(command, check=True, capture_output=True, timeout=20)
    finally:
        for path in directory.iterdir():
            if path.is_file():
                path.chmod(0o600)
    return ca, cert, key


class ProviderDefaultEndpointFixture:
    """Context manager with bounded socket cleanup and auditable TLS identity."""

    def __init__(self, directory, api_key, keys=(), *, certificate_files=None):
        self.ca, self.cert, self.key = certificate_files or certificates(directory)
        self.events, self.handler_errors = [], []
        self._condition = threading.Condition()
        self._connections = set()
        self._active = 0
        self._started = self._closed = False
        # Reuse the existing strict protocol's closure/state and handler. Its
        # HTTP listener is immediately closed and never serves any requests.
        self.protocol = RunPodFixture(api_key, keys)
        protocol_handler = self.protocol.server.RequestHandlerClass
        self.protocol.server.server_close()
        self._protocol_lock = threading.Lock()
        owner = self
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(self.cert, self.key)
        tls.set_alpn_protocols(['http/1.1'])

        def sni(connection, hostname, _context):
            owner.events.append({'event': 'sni', 'hostname': hostname, 'accepted': hostname == HOST})
            if hostname != HOST:
                return ssl.ALERT_DESCRIPTION_UNRECOGNIZED_NAME

        tls.set_servername_callback(sni)

        class TLSHandler(protocol_handler):
            timeout = 2

            def respond(self):
                hosts = self.headers.get_all('Host', [])
                if len(hosts) != 1 or hosts[0] not in (HOST, AUTHORITY):
                    return self.refuse(421, 'Host does not match CONNECT authority')
                if not self.path.startswith('/') or self.path.startswith('//'):
                    return self.refuse(400, 'Only origin-form request targets are accepted')
                lengths = self.headers.get_all('Content-Length', [])
                if self.headers.get('Transfer-Encoding') is not None or len(lengths) > 1:
                    return self.refuse(400, 'Ambiguous request framing')
                if lengths and (not lengths[0].isascii() or not lengths[0].isdecimal()):
                    return self.refuse(400, 'Invalid Content-Length')
                length = int(lengths[0]) if lengths else 0
                # Authentication failures in the reused protocol intentionally
                # do not parse JSON. Consume framed bytes first so closing TLS
                # cannot reset a valid 401 while the peer is still sending its
                # body. The original bytes remain the protocol's JSON oracle.
                body = self.rfile.read(length)
                if len(body) != length:
                    return self.refuse(400, 'Incomplete request body')
                self.rfile = io.BytesIO(body)
                self.path = '/proxy-a' + self.path
                with owner._protocol_lock:
                    # RunPodFixture rejects unknown paths, queries, methods,
                    # credentials and invalid key replacement bodies.
                    super().respond()

            def refuse(self, status, detail):
                owner.events.append({'event': 'http-rejected', 'method': self.command,
                                     'path': self.path, 'status': status})
                body = json.dumps({'status': status, 'detail': detail}).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/problem+json')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            do_GET = do_PUT = do_POST = do_DELETE = do_PATCH = do_OPTIONS = do_HEAD = respond

        class ProxyHandler(BaseHTTPRequestHandler):
            timeout = 2

            def do_CONNECT(self):
                hosts = self.headers.get_all('Host', [])
                accepted = (self.path == AUTHORITY and len(hosts) == 1
                            and hosts[0] in (HOST, AUTHORITY)
                            and self.headers.get('Transfer-Encoding') is None
                            and self.headers.get_all('Content-Length', []) in ([], ['0']))
                owner.events.append({'event': 'connect', 'authority': self.path, 'accepted': accepted})
                self.close_connection = True
                if not accepted:
                    self.send_error(403, 'CONNECT target is not allowed')
                    return
                self.send_response(200, 'Connection Established')
                self.end_headers()
                self.wfile.flush()
                encrypted = None
                try:
                    encrypted = tls.wrap_socket(self.connection, server_side=True, do_handshake_on_connect=False)
                    with owner._condition:
                        owner._connections.discard(self.connection)
                        owner._connections.add(encrypted)
                    encrypted.do_handshake()
                    owner.events.append({'event': 'tls', 'alpn': encrypted.selected_alpn_protocol()})
                    TLSHandler(encrypted, self.client_address, self.server)
                except (OSError, ssl.SSLError) as error:
                    owner.events.append({'event': 'connection-ended', 'type': type(error).__name__})
                finally:
                    if encrypted is not None:
                        encrypted.close()
                        with owner._condition:
                            owner._connections.discard(encrypted)
                            owner._condition.notify_all()

            def refuse(self):
                owner.events.append({'event': 'proxy-method-rejected', 'method': self.command})
                self.close_connection = True
                self.send_error(405, 'Only CONNECT is supported')

            do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_OPTIONS = do_HEAD = refuse

            def log_message(self, *_args):
                pass

        class Server(ThreadingHTTPServer):
            daemon_threads = False

            def get_request(self):
                connection, address = super().get_request()
                with owner._condition:
                    owner._connections.add(connection)
                return connection, address

            def process_request_thread(self, request, address):
                with owner._condition:
                    owner._active += 1
                try:
                    super().process_request_thread(request, address)
                finally:
                    with owner._condition:
                        owner._connections.discard(request)
                        owner._active -= 1
                        owner._condition.notify_all()

            def handle_error(self, request, address):
                owner.handler_errors.append('unexpected proxy handler exception')

        self.server = Server(('127.0.0.1', 0), ProxyHandler)
        self.address = self.server.server_address
        self.proxy_url = f'http://127.0.0.1:{self.address[1]}'
        self.thread = threading.Thread(target=lambda: self.server.serve_forever(poll_interval=0.05), daemon=True)

    @property
    def requests(self):
        records = copy.deepcopy(self.protocol.requests)
        for record in records:
            record['path'] = record['path'].removeprefix('/proxy-a')
        return records

    @property
    def active_handlers(self):
        with self._condition:
            return self._active

    @property
    def active_sockets(self):
        with self._condition:
            return len(self._connections)

    def child_environment(self):
        directory = Path(self.ca).parent / 'empty-ca-directory'
        directory.mkdir(mode=0o700, exist_ok=True)
        return {'HTTPS_PROXY': self.proxy_url, 'https_proxy': self.proxy_url,
                'HTTP_PROXY': self.proxy_url, 'http_proxy': self.proxy_url,
                'NO_PROXY': '127.0.0.1,localhost,::1', 'no_proxy': '127.0.0.1,localhost,::1',
                'ALL_PROXY': None, 'all_proxy': None,
                'SSL_CERT_FILE': str(self.ca), 'SSL_CERT_DIR': str(directory)}

    def __enter__(self):
        self.thread.start()
        self._started = True
        return self

    def stop(self):
        if not self._closed:
            if self._started:
                self.server.shutdown()
            with self._condition:
                connections = list(self._connections)
            for connection in connections:
                try:
                    connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass  # Peer closure is the expected alternative to shutdown.
                connection.close()
            self.server.server_close()
            if self._started:
                self.thread.join(timeout=5)
            self._closed = True
        clean = (not self.thread.is_alive() and self.server.socket.fileno() == -1
                 and self.active_handlers == 0 and self.active_sockets == 0)
        if not clean or self.handler_errors:
            raise AssertionError('RunPod CONNECT fixture did not close cleanly: '
                                 f'handlers={self.active_handlers}, sockets={self.active_sockets}, '
                                 f'errors={self.handler_errors!r}')
        return True

    def __exit__(self, *_args):
        self.stop()
