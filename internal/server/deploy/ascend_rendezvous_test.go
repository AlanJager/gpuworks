package deploy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAscendV1RendezvousAcrossRealProcesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("pathname UNIX sockets are required")
		}
		t.Skip("pathname UNIX sockets are unavailable")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("python3 is required by the compatibility script")
		}
		t.Skip("python3 is unavailable")
	}
	script := ascendV030RC2A3CompatScript
	start := strings.Index(script, "def _gpuworks_process_rendezvous(")
	end := strings.Index(script, "\ndef _gpuworks_npu_fill_workaround()")
	if start < 0 || end <= start {
		t.Fatalf("cannot extract the production rendezvous helper: %d, %d", start, end)
	}
	// Keep the socket's parent short: the production identity uses all 64 hex
	// digits of SHA-256, and macOS has a shorter sockaddr_un path than Linux.
	root, err := os.MkdirTemp("/tmp", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	harness := filepath.Join(root, "rendezvous_test.py")
	source := strings.Replace(ascendRendezvousPythonHarness, "PRODUCTION_RENDEZVOUS_HELPER", script[start:end], 1)
	if err := os.WriteFile(harness, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, harness)
	cmd.Env = make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "TMPDIR=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "TMPDIR="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("real-process rendezvous timed out: %v\n%s", ctx.Err(), out)
		}
		t.Fatalf("real-process rendezvous failed: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "gpuworks-v1-") {
			t.Fatalf("rendezvous left %s behind", entry.Name())
		}
	}
}

const ascendRendezvousPythonHarness = `import errno
import glob
import hashlib
import hmac
import os
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import zlib

PRODUCTION_RENDEZVOUS_HELPER


def rendezvous_parts(key, phase, generation):
    secret = bytes.fromhex(generation)
    key_bytes = key.encode("utf-8")
    phase_bytes = phase.encode("utf-8")
    material = (
        len(secret).to_bytes(8, "big") + secret
        + len(key_bytes).to_bytes(8, "big") + key_bytes + phase_bytes
    )
    identity = hashlib.sha256(material).digest()
    token = hmac.new(secret, b"GPUWorks rendezvous token" + identity, hashlib.sha256).digest()
    release = hmac.new(secret, b"GPUWorks rendezvous release" + token, hashlib.sha256).digest()
    path = os.path.join(tempfile.gettempdir(), "gpuworks-v1-" + identity.hex() + ".sock")
    return path, token, release


def start_process(rank, world_size, key, phases, generation):
    env = os.environ.copy()
    env["GPUWORKS_V1_RENDEZVOUS_GENERATION"] = generation
    return subprocess.Popen(
        [sys.executable, os.path.abspath(__file__), "worker", str(rank), str(world_size), key] + list(phases),
        env=env,
    )


def start_group(world_size, key, phases, generation):
    return [start_process(rank, world_size, key, phases, generation) for rank in range(world_size)]


def assert_socket_cleanup():
    leftovers = glob.glob(os.path.join(tempfile.gettempdir(), "gpuworks-v1-*.sock"))
    if leftovers:
        raise AssertionError(f"rendezvous sockets remain: {leftovers}")


def wait_groups(*groups):
    processes = [process for group in groups for process in group]
    deadline = time.monotonic() + 60
    try:
        for process in processes:
            process.wait(timeout=max(0.0, deadline - time.monotonic()))
        failed = [process for process in processes if process.returncode != 0]
        if failed:
            raise AssertionError(f"rendezvous processes failed: {[(p.pid, p.returncode) for p in failed]}")
        assert_socket_cleanup()
    finally:
        for process in processes:
            if process.poll() is None:
                process.kill()
        for process in processes:
            process.wait()


def connect_unix(path):
    client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    deadline = time.monotonic() + 15
    while True:
        try:
            client.settimeout(2)
            client.connect(path)
            return client
        except OSError:
            if time.monotonic() >= deadline:
                client.close()
                raise
            time.sleep(0.05)


def receive_raw(conn, size):
    result = bytearray()
    while len(result) < size:
        chunk = conn.recv(size - len(result))
        if not chunk:
            raise AssertionError("socket closed during handshake")
        result.extend(chunk)
    return bytes(result)


def fragmented_server_read(key, phase):
    generation = secrets.token_hex(32)
    path, token, release = rendezvous_parts(key, phase, generation)
    process = start_process(0, 2, key, (phase,), generation)
    client = None
    completed = False
    try:
        client = connect_unix(path)
        client.sendall((1).to_bytes(4, "big") + b"X" * len(token))
        if client.recv(1) != b"":
            raise AssertionError("rank zero accepted a wrong token")
        client.close()
        client = connect_unix(path)
        frame = (1).to_bytes(4, "big") + token
        for value in frame:
            client.sendall(bytes([value]))
            time.sleep(0.001)
        response = bytearray()
        while len(response) < len(release):
            chunk = client.recv(1)
            if not chunk:
                raise AssertionError("rank zero closed before the release")
            response.extend(chunk)
        if not hmac.compare_digest(bytes(response), release):
            raise AssertionError("rank zero returned the wrong release")
        completed = True
    finally:
        if client is not None:
            client.close()
        if not completed:
            process.kill()
            process.wait()
    wait_groups([process])


def rank_handshake(key, phase):
    generation = secrets.token_hex(32)
    path, token, release = rendezvous_parts(key, phase, generation)
    process = start_process(0, 3, key, (phase,), generation)
    clients = []
    completed = False
    try:
        wrong_rank = connect_unix(path)
        clients.append(wrong_rank)
        wrong_rank.sendall((0).to_bytes(4, "big") + token)
        if wrong_rank.recv(1) != b"":
            raise AssertionError("rank zero accepted rank zero as a peer")

        first = connect_unix(path)
        clients.append(first)
        first.sendall((1).to_bytes(4, "big") + token)
        duplicate = connect_unix(path)
        clients.append(duplicate)
        duplicate.sendall((1).to_bytes(4, "big") + token)
        if duplicate.recv(1) != b"":
            raise AssertionError("rank zero accepted a duplicate rank")

        second = connect_unix(path)
        clients.append(second)
        second.sendall((2).to_bytes(4, "big") + token)
        for client in (first, second):
            if not hmac.compare_digest(receive_raw(client, len(release)), release):
                raise AssertionError("rank zero released the wrong generation")
        completed = True
    finally:
        for client in clients:
            client.close()
        if not completed:
            process.kill()
            process.wait()
    wait_groups([process])


def owned_socket_cleanup(key, phase):
    generation = secrets.token_hex(32)
    path, token, release = rendezvous_parts(key, phase, generation)
    process = start_process(0, 2, key, (phase,), generation)
    client = connect_unix(path)
    replacement = None
    try:
        os.unlink(path)
        replacement = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        replacement.bind(path)
        replacement.listen(1)
        replacement_identity = os.lstat(path).st_ino
        client.sendall((1).to_bytes(4, "big") + token)
        if not hmac.compare_digest(receive_raw(client, len(release)), release):
            raise AssertionError("original rank zero released the wrong generation")
        process.wait(timeout=15)
        if process.returncode != 0:
            raise AssertionError(f"original rank zero failed: {process.returncode}")
        if os.lstat(path).st_ino != replacement_identity:
            raise AssertionError("rank zero cleanup removed another owner's socket")
    finally:
        client.close()
        if replacement is not None:
            replacement.close()
        if process.poll() is None:
            process.kill()
            process.wait()
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass
    assert_socket_cleanup()


def fragmented_client_read(key, phase):
    generation = secrets.token_hex(32)
    path, token, release = rendezvous_parts(key, phase, generation)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    process = None
    completed = False
    try:
        server.bind(path)
        server.listen(1)
        server.settimeout(15)
        process = start_process(1, 2, key, (phase,), generation)
        conn, _ = server.accept()
        try:
            peer_rank = int.from_bytes(receive_raw(conn, 4), "big")
            received = receive_raw(conn, len(token))
            if peer_rank != 1 or not hmac.compare_digest(received, token):
                raise AssertionError("client sent the wrong rank or generation token")
            for value in release:
                conn.sendall(bytes([value]))
                time.sleep(0.001)
            completed = True
        finally:
            conn.close()
    finally:
        server.close()
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass
        if process is not None:
            if completed:
                wait_groups([process])
            else:
                process.kill()
                process.wait()
    assert_socket_cleanup()


if __name__ == "__main__" and len(sys.argv) > 1 and sys.argv[1] == "worker":
    worker_rank = int(sys.argv[2])
    worker_world = int(sys.argv[3])
    worker_key = sys.argv[4]
    for worker_phase in sys.argv[5:]:
        _gpuworks_process_rendezvous(worker_rank, worker_world, worker_key, worker_phase)
    sys.exit(0)


if __name__ == "__main__":
    key = "tcp://127.0.0.1:29601"
    phases = (
        "upstream-groups",
        "new-group-0-enter-default",
        "new-group-0-exit-default",
        "ascend-groups",
    )
    repeated_generation = secrets.token_hex(32)
    wait_groups(start_group(16, key, phases, repeated_generation))
    wait_groups(start_group(16, key, phases, repeated_generation))

    wait_groups(
        start_group(16, key, ("same-key-and-phase",), secrets.token_hex(32)),
        start_group(16, key, ("same-key-and-phase",), secrets.token_hex(32)),
    )
    wait_groups(
        start_group(8, "key-a", ("shared-phase", "a-only"), secrets.token_hex(32)),
        start_group(8, "key-b", ("shared-phase", "b-only"), secrets.token_hex(32)),
        start_group(8, "key-a", ("another-phase",), secrets.token_hex(32)),
    )

    old_phase = "old-tcp-port"
    old_port = 18000 + zlib.crc32(f"{key}:{old_phase}".encode()) % 2000
    tcp = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        tcp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            tcp.bind(("127.0.0.1", old_port))
            tcp.listen(1)
        except OSError as error:
            if error.errno != errno.EADDRINUSE:
                raise
        wait_groups(start_group(16, key, (old_phase,), secrets.token_hex(32)))
    finally:
        tcp.close()

    stale_generation = secrets.token_hex(32)
    stale_key = "stale-socket"
    stale_path, _, _ = rendezvous_parts(stale_key, "generation", stale_generation)
    stale = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    stale.bind(stale_path)
    stale.close()
    wait_groups(start_group(4, stale_key, ("generation",), stale_generation))

    regular_generation = secrets.token_hex(32)
    regular_path, _, _ = rendezvous_parts("regular-file", "generation", regular_generation)
    os.environ["GPUWORKS_V1_RENDEZVOUS_GENERATION"] = regular_generation
    with open(regular_path, "x", encoding="utf-8") as regular:
        regular.write("do not unlink")
    try:
        try:
            _gpuworks_process_rendezvous(0, 1, "regular-file", "generation")
        except RuntimeError as error:
            if "not a UNIX socket" not in str(error):
                raise
        else:
            raise AssertionError("rank zero replaced a regular file")
        with open(regular_path, encoding="utf-8") as regular:
            if regular.read() != "do not unlink":
                raise AssertionError("rank zero changed a regular file")
    finally:
        os.unlink(regular_path)

    fragmented_server_read("short-server", "frames")
    fragmented_client_read("short-client", "frames")
    rank_handshake("rank-handshake", "frames")
    owned_socket_cleanup("owned-cleanup", "frames")
    assert_socket_cleanup()
    print("16-rank, multi-phase, generation-concurrent, repeated, stale, rank, short-frame checks passed")
`
