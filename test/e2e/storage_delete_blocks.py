"""Rows that make DeleteStorage refuse, and the browser confirmation that shows it."""
from __future__ import annotations

import sqlite3
from urllib.parse import urlsplit

from playwright.sync_api import expect
from platform_token_read_revoke import button, check


def storage_conn(server):
    conn = sqlite3.connect(server.work / 'server-data' / 'gpuworks.db', timeout=5)
    conn.execute('PRAGMA busy_timeout=5000')
    return conn


def storage_count(server, sql, args):
    conn = storage_conn(server)
    try:
        return conn.execute(sql, args).fetchone()[0]
    finally:
        conn.close()


def place_storage_reference(server, storage_id):
    """A deployment node requirement pins this storage, which DeleteStorage refuses."""
    conn = storage_conn(server)
    try:
        cur = conn.execute(
            'INSERT INTO deployments (name, model_id, served_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)',
            (f'e2e-storage-ref-{storage_id}', 0, f'e2e-storage-ref-{storage_id}',
             '2026-10-24 00:00:00', '2026-10-24 00:00:00'))
        dep_id = cur.lastrowid
        conn.execute(
            'INSERT INTO deployment_node_requirements (deployment_id, "key", value) VALUES (?, ?, ?)',
            (dep_id, 'gpuworks.ai/storage', str(storage_id)))
        conn.commit()
    finally:
        conn.close()
    check(storage_count(server, 'SELECT COUNT(*) FROM deployment_node_requirements WHERE "key" = ? AND value = ?',
                        ('gpuworks.ai/storage', str(storage_id))) == 1, 'storage reference missing after insert')
    return dep_id


def clear_storage_reference(server, storage_id, dep_id):
    conn = storage_conn(server)
    try:
        cur = conn.execute(
            'DELETE FROM deployment_node_requirements WHERE deployment_id = ? AND "key" = ? AND value = ?',
            (dep_id, 'gpuworks.ai/storage', str(storage_id)))
        conn.commit()
        check(cur.rowcount == 1, 'storage reference was not the one inserted')
        cur = conn.execute('DELETE FROM deployments WHERE id = ?', (dep_id,))
        conn.commit()
        check(cur.rowcount == 1, 'storage reference deployment was not the one inserted')
    finally:
        conn.close()


def place_storage_deleting_file(server, storage_id):
    """A model file still awaiting deletion acknowledgement refuses storage removal."""
    conn = storage_conn(server)
    try:
        conn.execute(
            'INSERT INTO model_files (model_id, storage_id, node_id, state, created_at, updated_at) VALUES (?, ?, 0, ?, ?, ?)',
            (0, storage_id, 'deleting', '2026-10-24 00:00:00', '2026-10-24 00:00:00'))
        conn.commit()
    finally:
        conn.close()
    check(storage_count(server, 'SELECT COUNT(*) FROM model_files WHERE storage_id = ? AND state = ?',
                        (storage_id, 'deleting')) == 1, 'deleting storage file missing after insert')


def clear_storage_deleting_file(server, storage_id):
    conn = storage_conn(server)
    try:
        cur = conn.execute('DELETE FROM model_files WHERE storage_id = ? AND state = ?', (storage_id, 'deleting'))
        conn.commit()
        check(cur.rowcount == 1, 'deleting storage file was not the one inserted')
    finally:
        conn.close()


def refuse_storage_delete(page, row, t, name, path, label):
    button(row, t['actions']['delete']).click()
    confirmation = t['actions']['deleteConfirm'].replace('{{name}}', name)
    popup = page.get_by_role('tooltip').filter(has_text=confirmation)
    expect(popup.get_by_text(t['actions']['deleteHelp'], exact=True)).to_be_visible()
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == 'DELETE') as received:
        button(popup, t['actions']['delete']).click()
    response = received.value
    # Status before .json(): a mutant that skips the idle check returns 204 with an empty body.
    check(response.status == 409, f'{label} status {response.status}')
    return response.json()


def allow_http_status(bad_http, console_errors, status):
    check(any(item.get('status') == status for item in bad_http), f'{status} was not recorded')
    check(any(str(status) in text for text in console_errors), f'{status} console line absent: {console_errors}')
    bad_http[:] = [item for item in bad_http if item.get('status') != status]
    console_errors[:] = [text for text in console_errors if str(status) not in text]
