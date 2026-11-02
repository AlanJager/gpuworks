"""Adversarial, authenticated RunPod error bodies over the strict base fixture.

Only two declared GET routes may echo the synthetic Bearer secret as a valid
problem+json detail. All other path/method/auth/body semantics delegate to the
existing RunPodFixture; no cloud/DNS or management interception is involved.
"""
import hashlib
import json

from provider_endpoint_fixture import RunPodFixture


class SecretErrorRunPodFixture(RunPodFixture):
    def __init__(self, api_key, marker, keys=(), *, key_error=True):
        super().__init__(api_key, keys)
        self.key_error = key_error
        self.pods_error = False
        self.emitted_errors = []
        owner = self
        original = self.server.RequestHandlerClass

        class Handler(original):
            def respond(self):
                authenticated = (self.headers.get('Authorization') == 'Bearer ' + owner.api_key
                                 and not owner.reject and not owner.forbidden)
                if self.command == 'GET' and authenticated:
                    if self.path == '/proxy-a/v2/account/ssh-keys' and owner.key_error:
                        return self.echo_error(400, 'Bad Request')
                    if self.path == '/proxy-a/v2/pods' and owner.pods_error:
                        return self.echo_error(403, 'Forbidden')
                return super().respond()

            def echo_error(self, status, title):
                body = {'title': title, 'status': status,
                        'detail': marker + ': upstream echoed Bearer ' + owner.api_key}
                data = json.dumps(body).encode('utf-8')
                owner.requests.append({'method': self.command, 'path': self.path, 'status': status,
                    'authorized': True, 'body': None, 'response': body})
                owner.emitted_errors.append({'method': self.command, 'path': self.path,
                    'authorization': self.headers.get('Authorization'), 'status': status,
                    'content_type': 'application/problem+json', 'body': body,
                    'body_sha256': hashlib.sha256(data).hexdigest(), 'bytes': len(data)})
                self.send_response(status)
                self.send_header('Content-Type', 'application/problem+json')
                self.send_header('Content-Length', str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = respond

        self.server.RequestHandlerClass = Handler
