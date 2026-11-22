"""ASGI middleware that pins the "model" of JSON requests to one model.

The engine answers under the model it serves, which on a Mac is a host path
(the agent turns the template's mount into the model's directory). The reply
goes back under the name the request asked for, so that path never reaches
the client: in a JSON body and in every streamed event alike.
"""
import json


class PinModel:
    def __init__(self, app, model):
        self.app = app
        self.model = model

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http" or scope.get("method") != "POST":
            return await self.app(scope, receive, send)
        headers = dict(scope.get("headers") or [])
        if b"application/json" not in headers.get(b"content-type", b""):
            return await self.app(scope, receive, send)
        chunks = []
        while True:
            message = await receive()
            chunks.append(message.get("body", b""))
            if not message.get("more_body"):
                break
        body = b"".join(chunks)
        try:
            payload = json.loads(body)
        except ValueError:
            payload = None
        asked = None
        if isinstance(payload, dict) and "model" in payload and payload["model"] != self.model:
            asked = payload["model"]
            payload["model"] = self.model
            body = json.dumps(payload).encode()
            scope = dict(scope)
            scope["headers"] = [(k, v) for k, v in scope.get("headers") or [] if k != b"content-length"]
            scope["headers"].append((b"content-length", str(len(body)).encode()))
        sent = False

        async def replay():
            # The body is handed over once; after that the app is listening for
            # the client going away (a streaming response does so while it
            # streams), which only the real receive knows. Answering
            # http.disconnect here would cut every stream short.
            nonlocal sent
            if sent:
                return await receive()
            sent = True
            return {"type": "http.request", "body": body, "more_body": False}

        if not isinstance(asked, str):
            return await self.app(scope, replay, send)
        served = json.dumps(self.model).encode()
        named = json.dumps(asked).encode()

        async def answer(message):
            # The name changes length, so a declared length would be wrong:
            # the server then frames the body itself.
            if message["type"] == "http.response.start":
                message = dict(message)
                message["headers"] = [(k, v) for k, v in message.get("headers") or [] if k.lower() != b"content-length"]
            elif message["type"] == "http.response.body" and served in message.get("body", b""):
                message = dict(message)
                message["body"] = message["body"].replace(served, named)
            await send(message)

        return await self.app(scope, replay, answer)
