"""Remote evidence validators fail closed; unit tests, not GPU coverage.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from remote_gpu_fixture import public_chat


class GenerationEvidenceTest(unittest.TestCase):
    def session(self):
        return SimpleNamespace(deployment={'served_name': 'owned-model'},
            artifacts=SimpleNamespace(redact=lambda text: text), evidence=lambda *_: None)

    def completion(self):
        return {'id': 'chat-real-response-shape', 'object': 'chat.completion', 'model': 'owned-model',
            'choices': [{'message': {'role': 'assistant', 'content': 'The sky is blue.'}, 'finish_reason': 'stop'}],
            'usage': {'prompt_tokens': 12, 'completion_tokens': 5, 'total_tokens': 17}}

    # Mainline: successful HTTP carrying empty/wrong/truncated evidence → reject generation claim.
    def test_nonstream_requires_model_text_stop_and_conserved_usage(self):
        for defect in ('model', 'empty', 'truncated', 'usage'):
            value = self.completion()
            if defect == 'model':
                value['model'] = 'unrelated-model'
            elif defect == 'empty':
                value['choices'][0]['message']['content'] = ' '
            elif defect == 'truncated':
                value['choices'][0]['finish_reason'] = 'length'
            else:
                value['usage']['total_tokens'] += 1
            with self.subTest(defect=defect), patch('remote_gpu_fixture.public_request', return_value=(200, 'application/json', json.dumps(value))):
                with self.assertRaises(AssertionError):
                    public_chat(self.session(), 'synthetic-unit-key')

    # Mainline: valid SSE chunks → one request ID, terminal event, generated text and exact usage.
    def test_stream_requires_terminal_event_and_one_request_identity(self):
        base = self.completion()
        chunks = [dict(id=base['id'], model=base['model'], choices=[{'delta': {'content': 'sky'}, 'finish_reason': None}]),
            dict(id=base['id'], model=base['model'], choices=[{'delta': {}, 'finish_reason': 'stop'}]),
            dict(id=base['id'], model=base['model'], choices=[], usage=base['usage'])]
        raw = ''.join('data: ' + json.dumps(c) + '\n\n' for c in chunks) + 'data: [DONE]\n\n'
        with patch('remote_gpu_fixture.public_request', return_value=(200, 'text/event-stream', raw)):
            result = public_chat(self.session(), 'synthetic-unit-key', stream=True)
            self.assertEqual(result['content'], 'sky')
            self.assertEqual(result['usage']['total_tokens'], 17)
        for bad in (raw.replace('data: [DONE]', ''), raw.replace(base['id'], 'mixed-id', 1)):
            with patch('remote_gpu_fixture.public_request', return_value=(200, 'text/event-stream', bad)):
                with self.assertRaises(AssertionError):
                    public_chat(self.session(), 'synthetic-unit-key', stream=True)

    # Mainline: 401 response → reject before interpreting any response as model output.
    def test_auth_denial_cannot_be_generation(self):
        with patch('remote_gpu_fixture.public_request', return_value=(401, 'application/json', '{"error":"denied"}')):
            with self.assertRaisesRegex(AssertionError, 'chat HTTP 401'):
                public_chat(self.session(), 'synthetic-unit-key')


class RemotePreflightTest(unittest.TestCase):
    # Mainline: GPU lanes without explicit storage identity → reject before login or host probes.
    def test_gpu_lanes_require_storage_before_browser_start(self):
        from remote_runtime_acceptance import main
        config = {'base_url': 'http://example.invalid', 'username': 'fixture',
            'password': 'synthetic-fixture-password', 'node_uuid': 'fixture-node',
            'model_id': 1, 'model_source_id': 'fixture-model', 'model_revision': 'fixture-revision',
            'image': 'fixture-image', 'gpu_index': 0}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'config.json'
            path.write_text(json.dumps(config))
            for lane in ('inference', 'network'):
                with self.subTest(lane=lane), patch('sys.argv', ['runner', '--config', str(path), '--case', lane]), \
                        patch('remote_runtime_acceptance.sync_playwright') as browser:
                    with self.assertRaisesRegex(AssertionError, 'missing explicit storage_source_id'):
                        main()
                    browser.assert_not_called()


if __name__ == '__main__':
    unittest.main()
