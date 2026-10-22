"""Pure preflight for the implemented Playground navigation subset.

E2E standard: test/e2e/browser-e2e-testing-standard.md
Unsupported business operations remain MISSING; this validator never runs them.
"""
import re
from urllib.parse import urlsplit
import yaml


NAVIGATION_PAGES = {'chat', 'vector', 'image', 'video', 'realtime'}
MODEL_PAGES = {'open_unsupported_chat', 'speech_form'}


class ManifestLoader(yaml.SafeLoader):
    """Reject duplicate keys instead of silently discarding declared semantics."""

    def construct_mapping(self, node, deep=False):
        self.flatten_mapping(node)
        keys = [self.construct_object(key, deep=deep) for key, _ in node.value]
        if any(not isinstance(key, str) for key in keys) or len(set(keys)) != len(keys):
            raise yaml.YAMLError('manifest keys must be unique strings')
        return super().construct_mapping(node, deep=deep)


def case_gaps(case):
    """Return unsupported or invalid semantics without consuming any input as evidence."""
    if not isinstance(case, dict):
        return ['case must be an object']
    gaps = []
    for key in case.keys() - {'id', 'browser', 'expect', 'reuse'}:
        gaps.append(f'case.{key}: not implemented')
        if key == 'deploy' and isinstance(case[key], dict):
            gaps.extend(f'deploy.{field}: placement/lifecycle not implemented' for field in case[key])
    identity = case.get('id')
    if not isinstance(identity, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]*', identity):
        gaps.append('id must be a nonempty safe case identifier')
    action = case.get('browser')
    if not isinstance(action, str) or action not in NAVIGATION_PAGES | MODEL_PAGES:
        gaps.append('browser: action not implemented')
    if isinstance(action, str) and action in MODEL_PAGES:
        if not isinstance(case.get('reuse'), str) or not case['reuse'].strip():
            gaps.append('reuse: explicit existing model required')
    elif 'reuse' in case:
        gaps.append('reuse: not consumed by this navigation action')
    expected = case.get('expect')
    if not isinstance(expected, dict) or not expected:
        gaps.append('expect must contain a supported explicit assertion')
        return gaps
    for key in expected.keys() - {'url_regex', 'visible_text'}:
        gaps.append(f'expect.{key}: not implemented')
    if 'url_regex' in expected:
        pattern = expected['url_regex']
        if not isinstance(pattern, str) or not pattern.strip():
            gaps.append('expect.url_regex must be a nonempty regex string')
        else:
            try:
                re.compile(pattern)
            except (re.error, OverflowError, RecursionError):
                gaps.append('expect.url_regex is invalid')
    if 'visible_text' in expected:
        texts = expected['visible_text']
        if not isinstance(texts, list) or not texts or any(not isinstance(s, str) or not s.strip() for s in texts):
            gaps.append('expect.visible_text must be a nonempty list of nonempty strings')
    return gaps


def validate_manifest(manifest):
    """Mainline: inspect every selected contract before any browser or remote action."""
    missing = []
    cases = []
    if not isinstance(manifest, dict):
        missing.append('manifest must be an object')
        manifest = {}
    missing.extend(f'manifest.{key}: not implemented' for key in manifest.keys() - {'defaults', 'cases'})
    defaults = manifest.get('defaults', {})
    if not isinstance(defaults, dict):
        missing.append('defaults must be an object')
    else:
        for key, value in defaults.items():
            if key not in {'base_url', 'username', 'password', 'artifact_dir'}:
                missing.append(f'defaults.{key}: not consumed by implemented actions')
            elif not isinstance(value, str) or not value.strip():
                missing.append(f'defaults.{key}: nonempty string required')
            elif key == 'base_url':
                try:
                    parsed = urlsplit(value)
                    valid = parsed.scheme in {'http', 'https'} and parsed.hostname and not parsed.username and not parsed.password
                    _ = parsed.port
                except ValueError:
                    valid = False
                if not valid:
                    missing.append('defaults.base_url: absolute HTTP(S) URL without credentials required')
    selected = manifest.get('cases')
    if not isinstance(selected, list) or not selected:
        missing.append('cases must be a nonempty list')
        selected = []
    seen = set()
    for index, case in enumerate(selected):
        gaps = case_gaps(case)
        identity = case.get('id') if isinstance(case, dict) else None
        if isinstance(identity, str):
            if identity in seen:
                gaps.append('id must be unique within the selected manifest')
            seen.add(identity)
        cases.append({'id': identity if isinstance(identity, str) else None, 'index': index, 'status': 'MISSING' if gaps or missing else 'NOT_RUN',
                      'missing': gaps})
    return {'status': 'MISSING' if missing or any(c['missing'] for c in cases) else 'NOT_RUN',
            'scope': 'manifest preflight; navigation assertions only; no GPU business execution',
            'external_actions_started': False, 'missing': missing, 'cases': cases}
