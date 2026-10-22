#!/usr/bin/env python3
"""Run a shell command on a gpuworks-managed node through its web terminal.

Some nodes the platform can reach have no inbound path for anyone else: the
agent dials out and the host sits behind NAT on a private address. The platform
already carries an interactive host shell to exactly those nodes, so the
calibration harness rides the same channel instead of needing SSH.

Terminal bytes travel as binary WebSocket frames and control messages as JSON
text (see internal/server/api/terminal.go), so this types the command as bytes
and reads bytes back. The upgrader also rejects a browser Origin that does not
match the host it was reached by, so none is sent.

    usage: node_exec.py <server> <token> <node-uuid> '<command>' [-v]
"""
import json
import sys
import time

import websocket  # websocket-client


def run(server, token, node_uuid, command, timeout=900, verbose=False):
    url = server.replace("http://", "ws://").replace("https://", "wss://")
    url = "%s/api/v1/nodes/%s/terminal" % (url.rstrip("/"), node_uuid)
    marker = "GPUWORKS_DONE_%d" % int(time.time() * 1000)
    # The pty echoes what is typed, so a marker written literally into the
    # command would match its own echo before the command ever ran. Splitting it
    # with quotes makes the shell print the marker while the echo shows the
    # quoted form.
    typed = marker.replace("_DONE_", "'_DONE_'")

    ws = websocket.create_connection(
        url, header=["Authorization: Bearer %s" % token],
        timeout=30, suppress_origin=True)
    # A quiet command can legitimately produce no bytes for minutes, so the
    # read timeout must outlast the quiet, not the whole run.
    ws.settimeout(150)

    # The shell announces itself before the first prompt; type only once it has.
    while True:
        frame = ws.recv()
        if isinstance(frame, bytes):
            break
        msg = json.loads(frame)
        if msg.get("type") in ("error", "exit"):
            raise RuntimeError("terminal refused: %s" % frame)
        if msg.get("type") == "info":
            continue

    # The pty takes the command a chunk at a time: a single large frame is
    # truncated part-way through, which silently drops the last lines of a
    # multi-line script.
    payload = ("%s; echo %s\n" % (command, typed)).encode()
    for offset in range(0, len(payload), 128):
        ws.send_binary(payload[offset:offset + 128])
        time.sleep(0.03)

    collected = []
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            frame = ws.recv()
        except Exception:
            break
        if not isinstance(frame, bytes):
            msg = json.loads(frame)
            if msg.get("type") == "exit":
                break
            continue
        text = frame.decode("utf-8", "replace")
        collected.append(text)
        if verbose:
            sys.stderr.write(text)
        if marker in "".join(collected):
            break

    try:
        ws.close()
    except Exception:
        pass

    text = "".join(collected)
    # Everything up to and including the echoed command line is the pty talking
    # back; keep only what the command printed.
    cut = text.find("\n")
    if cut >= 0 and command[:24] in text[:cut + 400]:
        rest = text[cut + 1:]
    else:
        rest = text
    idx = rest.find(marker)
    if idx >= 0:
        rest = rest[:idx]
    return strip_ansi(rest).strip()


def strip_ansi(text):
    """Drop CSI and OSC sequences.

    A pty emits both: CSI for colours and bracketed paste, OSC for the window
    title, which is terminated by BEL rather than by a final byte. Scanning for
    "the next letter in a set" runs past an OSC into real output, which is how
    an earlier version of this deleted the command's own results.
    """
    out, i = [], 0
    while i < len(text):
        ch = text[i]
        if ch == "\x1b" and i + 1 < len(text):
            nxt = text[i + 1]
            if nxt == "[":  # CSI ... final byte in @-~
                j = i + 2
                while j < len(text) and not ("@" <= text[j] <= "~"):
                    j += 1
                i = j + 1
                continue
            if nxt == "]":  # OSC ... BEL or ST
                j = i + 2
                while j < len(text) and text[j] not in ("\x07", "\x1b"):
                    j += 1
                i = j + 1
                continue
            i += 2
            continue
        if ch == "\x07":
            i += 1
            continue
        out.append(ch)
        i += 1
    return "".join(out).replace("\r\n", "\n").replace("\r", "")


if __name__ == "__main__":
    server, token, node, command = sys.argv[1:5]
    print(run(server, token, node, command, verbose="-v" in sys.argv))
