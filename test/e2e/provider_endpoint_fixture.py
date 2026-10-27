"""Selected RunPod REST v2 boundary for the account endpoint browser case.

Primary contract (checked 2026-10-21):
https://github.com/runpod/docs/blob/main/api-reference-v2/openapi.json
GET /v2/pods: ListPodsResponse; GET/PUT /v2/account/ssh-keys: SshKeys,
UpdateSshKeysRequest; bearerAuth and ErrorResponse (application/problem+json).
The proxy prefixes and optional exact raw query are fixture-owned routes,
not claims about native RunPod query parameters. By default every query is
rejected; a declared query must match byte-for-byte. Other routes/methods fail
closed; this fixture does not claim the remaining RunPod API or pod execution.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import re
import threading


class RunPodFixture:
    def __init__(self, api_key, keys=(), *, query=None):
        if query is not None and (not isinstance(query, str) or not query
                                  or any(character in query for character in ('#', '?', '\r', '\n'))):
            raise ValueError('query must be None or one nonempty raw query without delimiters')
        self.api_key, self.requests = api_key, []
        self.reject = self.forbidden = False
        self.keys = list(keys)
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            timeout = 5

            def respond(self):
                authorized = self.headers.get("Authorization") == "Bearer " + fixture.api_key
                path, separator, raw_query = self.path.partition('?')
                query_matches = (not separator if query is None else separator and raw_query == query)
                route = next((path[len(prefix):] for prefix in ("/proxy-a/v2", "/proxy-b/v2")
                              if query_matches and path in (prefix + "/pods", prefix + "/account/ssh-keys")), None)
                body, status, result = None, 200, None
                title, detail = "", ""
                if route is None:
                    status, title, detail = 404, "Not Found", "resource not found"
                elif self.command not in ({"GET", "PUT"} if route == "/account/ssh-keys" else {"GET"}):
                    status, title, detail = 405, "Method Not Allowed", "The requested method is not allowed for this path."
                elif not authorized or fixture.reject:
                    status, title = 401, "Unauthorized"
                    detail = "missing bearer token" if self.headers.get("Authorization") is None else "invalid bearer token"
                elif fixture.forbidden:
                    status, title, detail = 403, "Forbidden", "access denied"
                elif self.command == "PUT":
                    try:
                        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
                    except (json.JSONDecodeError, UnicodeDecodeError, ValueError):
                        status, title, detail = 400, "Bad Request", "request could not be processed"
                    else:
                        valid = (isinstance(body, dict) and set(body) == {"keys"} and isinstance(body["keys"], list)
                                 and all(isinstance(key, str) and re.fullmatch(r"(ssh|ecdsa|sk)-[^\s]+ [^\s]+([ \t][^\n\r]*)?", key)
                                         for key in body["keys"]))
                        if not valid:
                            status, title, detail = 422, "Unprocessable Entity", "Request validation failed."
                        else:
                            fixture.keys = list(body["keys"])
                if status == 200:
                    result = ({"keys": list(fixture.keys)} if route == "/account/ssh-keys" else
                              {"pods": [], "pagination": {"nextCursor": None, "hasNextPage": False}})
                else:
                    result = {"title": title, "status": status, "detail": detail}
                fixture.requests.append({"method": self.command, "path": self.path,
                                         "status": status, "authorized": authorized, "body": body, "response": result})
                data = json.dumps(result).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json" if status == 200 else "application/problem+json")
                if status == 405:
                    self.send_header("Allow", "GET, PUT" if route == "/account/ssh-keys" else "GET")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_PUT = do_POST = do_DELETE = do_PATCH = respond

            def log_message(self, *_args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        # server_close waits for every request handler, rather than only the listener.
        self.server.daemon_threads = False
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.base = f"http://127.0.0.1:{self.server.server_port}"

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        if self.thread.is_alive():
            raise AssertionError("RunPod fixture did not stop")
