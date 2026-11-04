"""Accept node removal on an explicitly prepared local Linux host.

Standard: test/e2e/browser-e2e-testing-standard.md
Requires an owned server, loopback OpenSSH service, empty gpuworks-agent.service,
matching server/agent binaries, and no compute GPU. The agent uses driver=none.
No management APIs, SSH commands, systemd operations, or agent processes are mocked.
Run as root: python3 test/e2e/node_removal_local_host.py --root <owned-install-dir>
  --base http://127.0.0.1:8804 --ssh-port 22222 --ssh-unit <owned-sshd.service>
  --browser-path <chromium> --locale zh
Leaves the server and a newly enrolled local node running for manual inspection.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import threading
import time
import traceback
import uuid
from urllib.parse import urlsplit

from playwright.sync_api import expect, sync_playwright
from local_browser_fixture import Artifacts, ROOT, digest
from node_removal import button, check

UNIT = "gpuworks-agent.service"
CONTRACT = "gpuworks/node/local-host-ssh-uninstall"


def run(*args, timeout=40):
    return subprocess.run(args, check=True, text=True, capture_output=True, timeout=timeout).stdout.strip()


def unit_state():
    text = run("systemctl", "show", UNIT, "-p", "MainPID", "-p", "ActiveState", "-p", "LoadState")
    return dict(line.split("=", 1) for line in text.splitlines())


def until(predicate, description, timeout=30):
    deadline = time.monotonic() + timeout
    while True:
        value = predicate()
        if value:
            return value
        if time.monotonic() >= deadline:
            raise AssertionError("timed out: " + description)
        threading.Event().wait(0.1)


class HostAcceptance:
    def __init__(self, args, page, context, artifacts, report):
        self.args, self.page, self.context = args, page, context
        self.artifacts, self.report = artifacts, report
        self.root = args.root.resolve()
        self.binary = self.root / "bin/gpuworks-agent"
        self.state = self.root / "agent-state"
        self.unit = Path("/etc/systemd/system") / UNIT
        self.child = None
        self.k = json.loads((ROOT / f"ui/src/i18n/locales/{args.locale}/nodes.json").read_text())
        self.marker = self.state / "models/acceptance-data.txt"
        self.binary_digest = digest(self.binary)

    def api(self, method, path, body=None):
        return self.context.request.fetch(self.args.base + path, method=method, data=body)

    def login(self):
        auth = json.loads((ROOT / f"ui/src/i18n/locales/{self.args.locale}/auth.json").read_text())
        self.page.goto(self.args.base + "/nodes")
        self.page.get_by_role("textbox", name=auth["username"], exact=True).fill("admin")
        self.page.get_by_label(auth["password"], exact=True).fill("gpuworks-test-password-2026")
        with self.page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/auth/login") as response:
            button(self.page, auth["submit"]).click()
        check(response.value.status == 200, "login failed")
        for cookie in self.context.cookies():
            self.artifacts.remember(cookie["value"])
        expect(self.page.get_by_role("heading", name=self.k["page"]["title"], exact=True)).to_be_visible()
        key = self.api("GET", "/api/v1/ssh-key")
        check(key.status == 200, "platform SSH key unavailable")
        # The dedicated sshd reads only this file; the host's normal SSH keys are unchanged.
        (self.root / "ssh/authorized_keys").write_text(key.json()["public_key"].rstrip() + "\n")
        check(unit_state()["LoadState"] == "not-found", "another standard agent service exists")
        self.report["preflight"] = "PASS"

    def enroll(self, suffix, mode="systemd", final=False):
        # Enrollment is setup through the real API. Deletion is driven through the browser.
        name = "local-node-89660" if final else f"e2e-{self.report['run_id']}-{suffix}"
        created = self.api("POST", "/api/v1/nodes", {"name": name, "server_url": self.args.base})
        check(created.status == 201, "node enrollment failed")
        data = created.json()
        token, node = data["token"], data["node"]
        self.artifacts.remember(token)
        self.node_uuid, self.name = node["uuid"], name
        self.report["current_node_uuid"] = self.node_uuid
        self.state.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.marker.parent.mkdir(parents=True, exist_ok=True)
        if not self.marker.exists():
            self.marker.write_text("owned acceptance data; must survive removal\n")
        self.marker_digest = digest(self.marker)
        token_file = self.root / "agent.env"
        token_file.write_text("GPUWORKS_TOKEN=" + token + "\n")
        token_file.chmod(0o600)
        argv = [str(self.binary), "run", "--server", self.args.base, "--state", str(self.state), "--driver", "none"]
        check(not self.unit.exists(), "refusing to overwrite an installed agent unit")
        if mode == "systemd":
            self.unit.write_text(f"""[Unit]
Description=GPUWorks ZSTAC-89660 owned local compute node
After=network.target
[Service]
Type=simple
EnvironmentFile={token_file}
ExecStart={' '.join(argv)}
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
""")
            run("systemctl", "daemon-reload")
            run("systemctl", "enable", "--now", UNIT)
        else:
            with (self.state / "console.log").open("a") as output:
                self.child = subprocess.Popen(["nohup", *argv], env={**os.environ, "GPUWORKS_TOKEN": token},
                    stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        def online():
            response = self.api("GET", self.node_path)
            current = response.json() if response.status == 200 else {}
            return current if current.get("state") == "online" else None
        current = until(online, "real agent connects")
        check(not current.get("runtime_driver"), "driver=none must report no runtime driver")
        self.pid = int(unit_state()["MainPID"]) if mode == "systemd" else self.child.pid
        check(self.pid > 0, "agent PID unavailable")
        actual_args = Path(f"/proc/{self.pid}/cmdline").read_bytes().split(b"\0")
        check(b"--driver" in actual_args and actual_args[actual_args.index(b"--driver") + 1] == b"none",
              "acceptance must not use a shared container runtime")
        ssh = self.api("PUT", self.node_path + "/ssh", {"host": "127.0.0.1", "port": self.args.ssh_port,
            "user": "root", "auth_mode": "platform_key", "probe": True})
        check(ssh.status == 200, "saving SSH credentials failed")
        self.ssh_before = ssh.json()
        self.page.reload()
        expect(self.row).to_have_count(1)
        self.page.screenshot(path=self.artifacts.directory / f"{suffix}-online.png")
        self.report["evidence"].append({"stage": "enrolled", "name": name, "node_uuid": self.node_uuid,
            "mode": mode, "pid": self.pid, "state": current["state"], "runtime_driver": current.get("runtime_driver", ""), "configured_driver": "none",
            "gpu_count": len(current.get("gpus", [])), "ssh": self.ssh_before})
        self.artifacts.write_json("report.json", self.report)

    @property
    def node_path(self):
        return "/api/v1/nodes/" + self.node_uuid

    @property
    def row(self):
        return self.page.get_by_role("row").filter(has_text=self.name)

    def open_delete(self):
        button(self.row, self.k["action"]["more"]).click()
        self.page.get_by_role("menuitem").filter(has=self.page.get_by_text(self.k["action"]["delete"], exact=True)).click()
        dialog = self.page.get_by_role("dialog", name=self.k["delete"]["confirm"], exact=True)
        expect(dialog).to_be_visible()
        expect(dialog.get_by_text(self.name, exact=True)).to_be_visible()
        return dialog

    def delete(self, dialog, force=False):
        checkbox = dialog.get_by_role("checkbox", name=self.k["delete"]["force"], exact=True)
        expect(checkbox).not_to_be_checked()
        if force:
            checkbox.check()
            expect(dialog.get_by_role("alert")).to_have_text(self.k["delete"]["forceWarning"])
        with self.page.expect_response(lambda r: urlsplit(r.url).path == self.node_path and r.request.method == "DELETE", timeout=45000) as observed:
            button(dialog, self.k["action"]["delete"]).click()
        response = observed.value
        check(urlsplit(response.url).query == ("force=true" if force else ""), "wrong deletion query")
        body = response.json()
        self.report["evidence"].append({"stage": "delete", "force": force, "node_uuid": self.node_uuid,
            "status": response.status, "body": body})
        return response.status, body

    def retained_data(self):
        check(digest(self.marker) == self.marker_digest, "model-directory data changed")
        check(digest(self.binary) == self.binary_digest, "agent binary removed or changed")
        check((self.state / "agent.log").is_file(), "agent log removed")

    def removed(self, systemd=True):
        check(self.api("GET", self.node_path).status == 404, "node deletion not persisted")
        check(self.api("GET", self.node_path + "/ssh").status == 404, "SSH record not removed")
        until(lambda: not Path(f"/proc/{self.pid}/cmdline").exists()
            or not Path(f"/proc/{self.pid}/cmdline").read_bytes(), "agent process exits")
        if systemd:
            check(not self.unit.exists(), "systemd unit still exists")
            check(unit_state()["LoadState"] == "not-found", "systemd still loads the unit")
        elif self.child:
            check(self.child.wait(timeout=5) == 0, "agent did not exit cleanly")
            self.child = None
        self.retained_data()
        self.page.reload()
        expect(self.row).to_have_count(0)
        self.report["evidence"].append({"stage": "verified_removal", "node_uuid": self.node_uuid,
            "old_pid": self.pid, "process_stopped": True, "unit_removed": systemd,
            "node_get": 404, "ssh_get": 404, "model_marker_sha256": self.marker_digest, "log_retained": True})

    def accept(self):
        # Mainline: systemd agent online -> SSH outage rejects removal without
        # losing state -> restore real SSH -> retry UI removal stops the process.
        self.enroll("systemd")
        self.artifacts.start_trace(self.context)
        dialog = self.open_delete()
        run("systemctl", "stop", self.args.ssh_unit)
        status, body = self.delete(dialog)
        check(status == 409 and body["code"] == "NODE_UNINSTALL_UNCONFIRMED", "SSH outage was not rejected")
        expect(dialog).to_be_visible()
        check(self.api("GET", self.node_path).status == 200, "failed deletion lost node")
        check(self.api("GET", self.node_path + "/ssh").status == 200, "failed deletion lost SSH credentials")
        check(int(unit_state()["MainPID"]) == self.pid, "failed deletion stopped the agent")
        self.page.screenshot(path=self.artifacts.directory / "ssh-outage-retained.png")
        run("systemctl", "start", self.args.ssh_unit)
        status, body = self.delete(dialog)
        check(status == 200 and body == {"status": "deleted", "agent_uninstalled": True}, "systemd uninstall not confirmed")
        self.removed()
        self.artifacts.stop_trace(self.context)

        # Mainline: online systemd agent -> unavailable SSH -> explicit force
        # removes management records while the same process and service survive.
        self.enroll("force")
        self.artifacts.start_trace(self.context)
        run("systemctl", "stop", self.args.ssh_unit)
        status, body = self.delete(self.open_delete(), force=True)
        check(status == 200 and body == {"status": "deleted", "agent_uninstalled": False}, "force removal failed")
        check(self.api("GET", self.node_path).status == 404, "force removal not persisted")
        check(self.api("GET", self.node_path + "/ssh").status == 404, "force removal kept SSH credentials")
        check(int(unit_state()["MainPID"]) == self.pid and self.unit.exists(), "force removal stopped the service")
        self.retained_data()
        self.page.reload()
        expect(self.row).to_have_count(0)
        self.report["evidence"].append({"stage": "verified_force", "node_uuid": self.node_uuid,
            "same_pid_still_running": self.pid, "unit_retained": True, "node_get": 404, "ssh_get": 404})
        self.artifacts.stop_trace(self.context)
        run("systemctl", "start", self.args.ssh_unit)
        check(str(self.binary) in self.unit.read_text() and str(self.state) in self.unit.read_text(), "unit ownership changed")
        run(str(self.binary), "uninstall", "--state", str(self.state))

        # Mainline: an actual detached agent, without a systemd unit, is also
        # stopped by a normal browser removal through real OpenSSH.
        self.enroll("nohup", mode="nohup")
        self.artifacts.start_trace(self.context)
        status, body = self.delete(self.open_delete())
        check(status == 200 and body == {"status": "deleted", "agent_uninstalled": True}, "detached process uninstall failed")
        self.removed(systemd=False)
        self.artifacts.stop_trace(self.context)
        self.enroll("final", final=True)
        self.report["final_node"] = {"uuid": self.node_uuid, "name": self.name, "pid": self.pid, "state": "online"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--base", required=True)
    parser.add_argument("--ssh-port", type=int, required=True)
    parser.add_argument("--ssh-unit", required=True)
    parser.add_argument("--browser-path", required=True)
    parser.add_argument("--locale", choices=["en", "zh"], default="zh")
    args = parser.parse_args()
    check(os.geteuid() == 0, "this explicit local-host acceptance requires root")
    check(urlsplit(args.base).hostname == "127.0.0.1", "only a loopback server is allowed")
    check(args.root.is_absolute() and " " not in str(args.root), "use an absolute owned directory without spaces")
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:6]
    artifacts = Artifacts(args.root / "evidence" / run_id)
    deployment = json.loads((args.root / "evidence/deployment.json").read_text())
    report = {"contract": CONTRACT, "run_id": run_id, "status": "NOT_RUN", "locale": args.locale,
        "lane": "local-host-real-systemd-openssh", "source_sha": deployment["source_sha"],
        "binaries": deployment["binaries"], "test_sha256": digest(Path(__file__)), "base_url": args.base,
        "evidence": [], "not_covered": ["GPU inference: host has no compute GPU", "macOS", "full test suites",
            "live managed inference busy-node rejection (covered by API unit tests)", "concurrent scheduler/recovery races"]}
    page_errors = []
    try:
        with sync_playwright() as play:
            browser = play.chromium.launch(executable_path=args.browser_path, headless=True)
            context = browser.new_context(locale="zh-CN" if args.locale == "zh" else "en-US", viewport={"width":1440,"height":900}, timezone_id="UTC")
            page = context.new_page()
            page.set_default_timeout(12000)
            page.on("pageerror", lambda e: page_errors.append(str(e)))
            report["browser_version"] = browser.version
            test = HostAcceptance(args, page, context, artifacts, report)
            try:
                test.login()
                test.accept()
                check(not page_errors, "browser errors: " + str(page_errors))
                for unit, baseline in deployment["unrelated_services"].items():
                    check(run("systemctl", "show", unit, "-p", "MainPID", "-p", "ActiveState") == baseline,
                          "unrelated service changed: " + unit)
                report["unrelated_services_unchanged"] = True
                report["status"] = "PASS"
            except Exception:
                report["status"] = "FAIL_TEST" if report.get("preflight") == "PASS" else "BLOCKED_INFRA"
                report["failure"] = traceback.format_exc()
                if not any(secret in page.content() for secret in artifacts.secrets if len(secret) > 8):
                    page.screenshot(path=artifacts.directory / "failure.png")
                report["dialog_snapshot"] = page.get_by_role("dialog").aria_snapshot() if page.get_by_role("dialog").count() == 1 else "no unique dialog"
            finally:
                artifacts.stop_trace(context)
                context.close()
                browser.close()
    except Exception:
        report["status"] = "BLOCKED_INFRA"
        report["failure"] = traceback.format_exc()
    finally:
        artifacts.write_json("report.json", report)
        artifacts.scan()
    print(json.dumps({"status":report["status"], "report":str(artifacts.directory/"report.json"), "final_node":report.get("final_node")}))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
