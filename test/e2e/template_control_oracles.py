"""Pure candidate oracles. Passing these tests is never browser evidence."""
from urllib.parse import parse_qs, urlsplit


def require(ok, message):
    if not ok:
        raise AssertionError(message)


def inside(a, b):
    return (a['width'] > 0 and a['height'] > 0 and a['x'] >= b['x'] - .5
            and a['y'] >= b['y'] - .5
            and a['x'] + a['width'] <= b['x'] + b['width'] + .5
            and a['y'] + a['height'] <= b['y'] + b['height'] + .5)


def overlap(a, b):
    return (min(a['x'] + a['width'], b['x'] + b['width']) > max(a['x'], b['x']) + .5
            and min(a['y'] + a['height'], b['y'] + b['height']) > max(a['y'], b['y']) + .5)


def geometry(boxes, viewport):
    required = {'drawer', 'header', 'title', 'actions', 'body', 'editor', 'save', 'reset'}
    require(set(boxes) == required, 'geometry region schema drift')
    require(viewport == {'width': 1440, 'height': 900}, 'wrong viewport')
    screen = dict(x=0, y=0, **viewport)
    for name, box in boxes.items():
        require(isinstance(box, dict) and inside(box, screen), f'{name} outside viewport')
        require(inside(box, boxes['drawer']), f'{name} outside drawer')
    for child in ('title', 'actions'):
        require(inside(boxes[child], boxes['header']), f'{child} outside header')
    for child in ('save', 'reset'):
        require(inside(boxes[child], boxes['actions']), f'{child} outside actions')
        require(inside(boxes[child], boxes['header']), f'{child} outside header')
        require(not overlap(boxes[child], boxes['editor']), f'{child} overlaps editor')
    require(inside(boxes['editor'], boxes['body']), 'editor outside body')
    require(not overlap(boxes['title'], boxes['actions']), 'title overlaps actions')
    require(not overlap(boxes['header'], boxes['body']), 'header overlaps body')
    require(not overlap(boxes['save'], boxes['reset']), 'actions overlap')


def import_wire(method, url, content_type, body, expected):
    path = urlsplit(url)
    require(method == 'POST' and path.path == '/api/v1/inference-templates/import', 'wrong import route')
    require(parse_qs(path.query) == {'overwrite': ['true']}, 'wrong overwrite query')
    require(content_type == 'application/yaml', 'wrong YAML content type')
    require(body == expected, 'submitted YAML changed')


def changed_title(yaml, old, new):
    import json
    import re
    # yaml.v3 may use a plain or quoted scalar. Require exactly one actual
    # model field and an independently read baseline value; never replace prose.
    fields = list(re.finditer(r'(?m)^( +display_title: )([^\n]*)$', yaml))
    require(len(fields) == 1, 'canonical title shape drift')
    field = fields[0]
    require(field[2] in (old, json.dumps(old, ensure_ascii=False)), 'canonical title value drift')
    require(re.fullmatch(r'e2e-[A-Za-z0-9 -]+', new) is not None, 'unsafe replacement title')
    return yaml[:field.start(2)] + new + yaml[field.end(2):]


def cleanup_owned(owned, fetch, remove, record):
    """Always attempt every owned model; removal requires independent absence."""
    ok = True
    for model_id in list(owned):
        try:
            status, model = fetch(model_id)
            if status != 404:
                require(status == 200 and model['source'] == 'user', 'cleanup refuses non-user model')
                specs = model['specs']
                require(specs and all(s['source'] == 'user' for s in specs), 'cleanup refuses mixed specs')
                for spec in specs:
                    require(remove(model_id, spec['name']) == 200, 'spec cleanup failed')
                require(fetch(model_id)[0] == 404, 'model cleanup not independently absent')
            owned.remove(model_id)
            record({'model': model_id, 'ok': True})
        except Exception as error:
            ok = False
            record({'model': model_id, 'ok': False, 'error_type': type(error).__name__})
    return ok


def finish_stage(report, label, action):
    """Cleanup exceptions never erase the original business exception."""
    import traceback
    try:
        require(action() is True, label + ' did not confirm success')
        report.setdefault('cleanup_steps', []).append({'stage': label, 'ok': True})
        return True
    except Exception:
        report.setdefault('failure', traceback.format_exc())
        report.setdefault('cleanup_steps', []).append({'stage': label, 'ok': False})
        report.setdefault('cleanup_errors', []).append({'stage': label, 'error': traceback.format_exc()})
        return False


def archive_members(raw):
    """Compare file bytes independently of Go/Python gzip header differences."""
    import io
    import tarfile
    from pathlib import PurePosixPath
    files = {}
    with tarfile.open(fileobj=io.BytesIO(raw), mode='r:gz') as archive:
        for entry in archive:
            path = PurePosixPath(entry.name)
            require(entry.isfile() and not path.is_absolute() and '..' not in path.parts
                    and entry.name not in files, 'invalid/duplicate package member')
            files[entry.name] = archive.extractfile(entry).read()
    require(files and 'manifest.yaml' in files, 'package manifest missing')
    return files
