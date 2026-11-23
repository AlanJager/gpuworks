"""python3 -m unittest discover -s build/host-envs/adapters (no dependencies)."""
import asyncio
import json
import unittest

from gpuworks_adapters.rewrite import PinModel


async def call(app, method, content_type, body, chunks=1):
    seen = {}

    async def inner(scope, receive, send):
        parts = []
        while True:
            message = await receive()
            parts.append(message.get("body", b""))
            if not message.get("more_body"):
                break
        seen["body"] = b"".join(parts)
        seen["headers"] = dict(scope["headers"])

    scope = {"type": "http", "method": method, "headers": [(b"content-type", content_type), (b"content-length", str(len(body)).encode())]}
    size = max(1, len(body) // chunks)
    pieces = [body[i:i + size] for i in range(0, len(body), size)] or [b""]
    queue = [{"type": "http.request", "body": p, "more_body": i < len(pieces) - 1} for i, p in enumerate(pieces)]

    async def receive():
        return queue.pop(0)

    await PinModel(inner, "/models/ocr")(scope, receive, None)
    return seen


class PinModelTest(unittest.TestCase):
    def test_rewrites_the_model_of_a_json_request(self):
        body = json.dumps({"model": "ocr-deployment", "messages": []}).encode()
        seen = asyncio.run(call(None, "POST", b"application/json", body, chunks=3))
        self.assertEqual(json.loads(seen["body"])["model"], "/models/ocr")
        self.assertEqual(seen["headers"][b"content-length"], str(len(seen["body"])).encode())

    def test_leaves_other_requests_alone(self):
        for method, ctype, body in (
            ("POST", b"application/json", json.dumps({"model": "/models/ocr"}).encode()),
            ("POST", b"application/json", b"not json"),
            ("POST", b"multipart/form-data; boundary=x", b"--x--"),
            ("GET", b"application/json", b""),
        ):
            seen = asyncio.run(call(None, method, ctype, body))
            self.assertEqual(seen["body"], body)


    def test_a_streaming_response_keeps_running_until_the_client_leaves(self):
        # A streaming response listens for http.disconnect while it streams
        # (Starlette does): after the body, receive must wait for the real
        # client instead of reporting that it has gone.
        async def run():
            client_gone = asyncio.Event()
            sent = []

            async def receive():
                if not sent:
                    sent.append(True)
                    return {"type": "http.request", "body": json.dumps({"model": "x"}).encode(), "more_body": False}
                await client_gone.wait()
                return {"type": "http.disconnect"}

            chunks = []

            async def streaming_app(scope, receive, send):
                await receive()  # the body
                listener = asyncio.ensure_future(receive())
                for i in range(5):
                    await asyncio.sleep(0)
                    if listener.done():
                        return  # cut short, as Starlette's StreamingResponse would
                    chunks.append(i)
                client_gone.set()
                await listener

            scope = {"type": "http", "method": "POST", "headers": [(b"content-type", b"application/json")]}
            await PinModel(streaming_app, "/m")(scope, receive, None)
            return chunks

        self.assertEqual(asyncio.run(run()), [0, 1, 2, 3, 4])


class AnswerUnderTheAskedNameTest(unittest.TestCase):
    """The engine echoes the model it serves (a host path on a Mac); the client
    gets back the name it asked for."""

    def run_app(self, request, reply_chunks, start_headers):
        async def app(scope, receive, send):
            await receive()
            await send({"type": "http.response.start", "status": 200, "headers": start_headers})
            for i, chunk in enumerate(reply_chunks):
                await send({"type": "http.response.body", "body": chunk, "more_body": i < len(reply_chunks) - 1})

        sent = []

        async def send(message):
            sent.append(message)

        async def receive():
            return {"type": "http.request", "body": json.dumps(request).encode(), "more_body": False}

        scope = {"type": "http", "method": "POST", "headers": [(b"content-type", b"application/json")]}
        asyncio.run(PinModel(app, "/Users/someone/models/m")(scope, receive, send))
        return sent

    def test_a_json_reply_names_the_asked_model_and_drops_the_stale_length(self):
        body = json.dumps({"id": "1", "model": "/Users/someone/models/m", "choices": []}).encode()
        sent = self.run_app({"model": "public-name"}, [body],
                            [(b"content-type", b"application/json"), (b"content-length", str(len(body)).encode())])
        self.assertNotIn(b"content-length", dict(sent[0]["headers"]))
        reply = json.loads(sent[1]["body"])
        self.assertEqual(reply["model"], "public-name")
        self.assertNotIn(b"/Users/someone", sent[1]["body"])

    def test_every_streamed_event_names_the_asked_model(self):
        events = [b'data: {"model":"/Users/someone/models/m","choices":[{"delta":{"content":"a"}}]}\n\n',
                  b'data: {"model": "/Users/someone/models/m", "choices": []}\n\n',
                  b"data: [DONE]\n\n"]
        sent = self.run_app({"model": "public-name", "stream": True}, events, [(b"content-type", b"text/event-stream")])
        bodies = [m["body"] for m in sent[1:]]
        self.assertEqual(len(bodies), 3)
        self.assertTrue(all(b"/Users/someone" not in b for b in bodies))
        self.assertIn(b'"model":"public-name"', bodies[0])
        self.assertIn(b'"model": "public-name"', bodies[1])
        self.assertEqual(bodies[2], b"data: [DONE]\n\n")

    def test_a_request_already_naming_the_served_model_is_answered_untouched(self):
        body = json.dumps({"model": "/Users/someone/models/m"}).encode()
        headers = [(b"content-length", str(len(body)).encode())]
        sent = self.run_app({"model": "/Users/someone/models/m"}, [body], headers)
        self.assertEqual(sent[0]["headers"], headers)
        self.assertEqual(sent[1]["body"], body)


if __name__ == "__main__":
    unittest.main()
