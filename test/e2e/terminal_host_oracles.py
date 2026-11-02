"""Independent host-terminal bytes, policy and audit oracles; no fake PTY."""
import json
from urllib.parse import parse_qs, urlsplit


def require(value, message):
    if not value:
        raise AssertionError(message)


POLICY = {'enabled': True, 'idle_timeout_min': 1, 'grace_sec': 1,
          'max_per_node': 1, 'max_global': 2}
NODE_BUSY = 'too many terminal sessions on this node'
GLOBAL_BUSY = 'too many terminal sessions on this platform'
OFFLINE = 'node is not connected'
TEXT = {
    'en': {'host': 'Host terminal', 'open': 'Connected', 'exited': 'Ended',
           'error': 'Unavailable', 'ended': 'Session ended', 'reconnect': 'Reconnect',
           'title': 'Terminal', 'permission': 'Administrator permissions are required.'},
    'zh': {'host': '宿主机终端', 'open': '已连接', 'exited': '已结束',
           'error': '不可用', 'ended': '会话结束', 'reconnect': '重新连接',
           'title': '终端', 'permission': '此操作需要管理员权限。'},
}
AUDIT_KEYS = {'id', 'username', 'node_uuid', 'target', 'client_ip', 'started_at',
              'bytes_in', 'bytes_out', 'active'}
CLOSED_KEYS = {'ended_at', 'duration_sec', 'close_reason'}


def socket_contract(url, node):
    parts = urlsplit(url)
    query = parse_qs(parts.query, keep_blank_values=True)
    require(parts.path == '/api/v1/nodes/' + node + '/terminal', 'wrong host WS node')
    require(set(query) == {'target', 'rows', 'cols'} and query['target'] == ['host'],
            'host WS must have exact target/geometry and no ref/token')
    for key in ('rows', 'cols'):
        require(len(query[key]) == 1 and query[key][0].isdigit()
                and 1 <= int(query[key][0]) <= 1000, 'invalid host WS geometry')
    return {key: int(query[key][0]) for key in ('rows', 'cols')}


def policy_contract(value):
    require(set(value) == set(POLICY), 'terminal policy keys differ')
    require(type(value['enabled']) is bool and all(type(value[k]) is int for k in POLICY if k != 'enabled'),
            'terminal policy types differ')
    require(value['enabled'] is True and 1 <= value['idle_timeout_min'] <= 1440
            and 1 <= value['grace_sec'] <= 120 and 1 <= value['max_per_node'] <= 64
            and value['max_per_node'] <= value['max_global'] <= 512, 'invalid terminal policy')
    return value


def identity(raw):
    import re
    m = re.fullmatch(r'(\d+) \((.*)\) (.*)', raw.strip())
    require(m is not None, 'invalid host /proc identity')
    fields = m[3].split()
    require(int(m[1]) > 1 and len(fields) >= 20 and fields[19].isdigit(), 'invalid host process fields')
    return int(m[1]), int(fields[19])


class Frames:
    def __init__(self):
        self.urls, self.binary, self.controls, self.resizes = [], [], [], []
        self.input_bytes = 0

    def received(self, payload):
        if isinstance(payload, str):
            value = json.loads(payload)
            require(value.get('type') in ('info', 'exit', 'error'), 'unknown WS control')
            self.controls.append(value)
        else:
            require(isinstance(payload, bytes), 'WS binary must be actual bytes')
            self.binary.append(payload)

    def sent(self, payload):
        if isinstance(payload, str):
            msg = json.loads(payload)
            require(set(msg) == {'type', 'rows', 'cols'} and msg['type'] == 'resize'
                    and all(type(msg[k]) is int and 1 <= msg[k] <= 1000 for k in ('rows', 'cols')),
                    'unexpected client text/resize frame')
            self.resizes.append(msg)
        else:
            require(isinstance(payload, bytes), 'client input must be binary bytes')
            self.input_bytes += len(payload)

    def attach(self, socket):
        self.urls.append(socket.url)
        socket.on('framereceived', self.received)
        socket.on('framesent', self.sent)

    def output(self):
        return b''.join(self.binary)

    def split_payload(self, payload):
        data = self.output()
        offset = data.find(payload)
        require(offset >= 0, 'exact multibyte payload absent from real binary output')
        boundaries, total = set(), 0
        for chunk in self.binary:
            total += len(chunk)
            boundaries.add(total)
        require(offset + 1 in boundaries, 'first UTF8 character did not actually cross a WS frame')
        require(payload.decode('utf-8').encode('utf-8') == payload, 'expected UTF8 is invalid')


def audit_contract(row, node, username, active, reason=None):
    keys = AUDIT_KEYS | ({'ref'} if 'ref' in row else set()) | (CLOSED_KEYS if not active else set())
    # Zero duration is omitted by the DTO even for a short ended session.
    if not active and 'duration_sec' not in row:
        keys.remove('duration_sec')
    require(set(row) == keys, 'audit schema includes content/credentials or unknown keys')
    require(type(row['id']) is int and row['id'] > 0 and row['username'] == username
            and row['node_uuid'] == node and row['target'] == 'host' and row.get('ref', '') == ''
            and row['client_ip'] == '127.0.0.1' and row['active'] is active, 'audit subject/host target differs')
    require(all(type(row[k]) is int and row[k] >= 0 for k in ('bytes_in', 'bytes_out')), 'invalid audit byte counters')
    if reason:
        require(row.get('close_reason') == reason and bool(row.get('ended_at')), 'wrong terminal close result')
