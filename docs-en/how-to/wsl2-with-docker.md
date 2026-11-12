# Run GPUWorks on Windows WSL2 with Docker

This page explains how to install the control plane and a local GPU node inside Windows WSL2 (Ubuntu), use **Docker + the NVIDIA Container Toolkit** as the inference runtime, and complete a small-model deploy and call.

**When to use this**: you only have a Windows workstation or laptop with an NVIDIA GPU, and you want a local GPUWorks trial with the browser at `http://localhost:8800`.

**Prerequisites**:

- Install the latest NVIDIA driver on **Windows**. Do **not** install a Linux NVIDIA driver inside WSL (WSL uses the Windows driver).
- WSL2 is enabled with an Ubuntu distro (22.04/24.04 recommended).
- systemd is enabled for the distro (`/etc/wsl.conf` contains `[boot] systemd=true`; then run `wsl --shutdown` and reopen the distro).
- Leave roughly **50 GiB** free on the Windows system drive for a small-model smoke test (a runner image expanded in Docker can be around 30 GiB, plus weights and WSL VHDX growth).
- Validate with a **small** model (for example Qwen3-1.7B class). A 16 GiB consumer GPU is not suitable for accepting multi-tens-of-GiB weight packs on this machine.

GPUWorks has no separate Windows installer: the control plane and the agent install inside the WSL distro the same way as on Linux. Compared with an ordinary Linux host, WSL needs two extra things: the inference runtime must be Docker, and the distro must stay running.

## Why Docker is required

On WSL2, if the agent does not find a usable Docker, it falls back to runveil/udocker. Starting a vLLM-class runner on that path tends to fail because CUDA compat `libcuda` inside the image does not work with WSL’s `/dev/dxg` path. A typical error is:

```text
RuntimeError: Failed to infer device type
```

So this guide requires:

1. Docker Engine and the NVIDIA Container Toolkit.
2. Agent logs showing `runtime driver detected driver=docker` before you deploy.

For the general runveil/udocker design, see [runveil and udocker](../explanation/runveil-and-udocker.md). This page only covers the path that works on WSL2.

## 1. Keep WSL running (so the browser does not lose the console)

Edit `%USERPROFILE%\.wslconfig` on Windows:

```ini
[wsl2]
memory=18GB
swap=2GB
processors=8
localhostForwarding=true
vmIdleTimeout=-1

[general]
instanceIdleTimeout=-1
```

Notes:

- `vmIdleTimeout=-1` only keeps the WSL **VM** from shutting down when idle.
- `instanceIdleTimeout=-1` keeps the Ubuntu **distro instance** from exiting when idle. Without it, `wsl -l -v` can show `Stopped` and `localhost:8800` becomes unreachable.
- Adjust memory and CPU to your machine. After editing, run `wsl --shutdown`, then reopen the distro.

Optional: a scheduled task that runs `wsl.exe -d <distro> --exec /bin/true` at logon and periodically as a backup wake-up.

Before opening the console, confirm:

```powershell
wsl -l -v
```

The distro should be `Running`.

## 2. Install Docker and the NVIDIA Container Toolkit in WSL

In a WSL terminal, install Docker Engine (follow Docker’s Ubuntu docs), then:

```bash
sudo systemctl enable --now docker
docker version
```

Install the NVIDIA Container Toolkit (follow NVIDIA’s docs) and configure Docker:

```bash
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

Verify the GPU is visible in a container:

```bash
docker run --rm --gpus all --entrypoint nvidia-smi ubuntu:24.04 -L
```

You should see the GPU listed. If Docker Hub pulls time out, configure a reachable mirror or use another accessible base image first.

Runner images are tens of GiB, and the more layers download at once, the more memory and disk pressure WSL takes. Limit Docker's concurrent downloads. `nvidia-ctk` has already written the NVIDIA runtime into `/etc/docker/daemon.json`, so the command below merges this key in instead of overwriting the file:

```bash
sudo python3 - <<'EOF'
import json, pathlib
path = pathlib.Path("/etc/docker/daemon.json")
config = json.loads(path.read_text()) if path.exists() else {}
config["max-concurrent-downloads"] = 2
path.write_text(json.dumps(config, indent=2) + "\n")
EOF
sudo systemctl restart docker
```

## 3. Install the control plane

In the same WSL distro:

```bash
bash <(curl -fsSL https://dl.gpuworks.ai/get.sh) --quiet --addr 0.0.0.0:8800
```

- `--addr 0.0.0.0:8800`: the default listen address is the WSL virtual NIC's address, which Windows `localhost` forwarding cannot reach, so listen on all addresses instead; the agent also connects through `127.0.0.1`. When WSL uses mirrored networking (`networkingMode=mirrored` in `.wslconfig`), this address exposes the console to your LAN: change the admin password first, and restrict inbound access to port 8800 with Windows Firewall.
- Drop `--quiet` if you want interactive prompts. See [Install and Upgrade the Control Plane](install-server.md) for more options.

Verify:

```bash
systemctl is-active gpuworks
curl -fsS http://127.0.0.1:8800/healthz
```

Open **http://localhost:8800** in a Windows browser. Log in with the `admin` password from the installer output (also written to `initial-credentials` under the data directory; see [Control Plane Installation Layout](../reference/install-layout.md)). Change the password after login and delete that file.

## 4. Connect the local node

1. Open **Compute nodes → Add node**.
2. Choose agent-initiated connect (`agent_dial`) and set the server URL to `http://127.0.0.1:8800`.
3. Run the generated install command in the same WSL terminal (`curl …/install.sh | sh -s -- --server … --token …`). Details: [Connect Nodes](connect-nodes.md).

Confirm the agent uses Docker:

```bash
journalctl -u gpuworks-agent -n 50 --no-pager | grep 'runtime driver'
```

Expect:

```text
runtime driver detected driver=docker
```

When Docker is not chosen, the `driver` in the same log line says why:

- `udocker (no docker daemon)`: Docker was not found. Make sure `docker.service` is `active` and `/var/run/docker.sock` exists, then run `sudo systemctl restart gpuworks-agent`.
- `udocker (docker daemon not answering)`: Docker is installed but not answering. The agent rechecks every minute and switches back once Docker answers and no instance is running on the node.
- `udocker (docker cannot use the accelerators: …)`: Docker cannot give the GPU to containers, and an earlier line reads `runtime driver passed over: it cannot give containers this node's accelerators`. Configure the NVIDIA Container Toolkit as in section 2 and restart Docker.

Once the node uses Docker, pin the runtime: open the node under **Compute Nodes** and choose `docker` in **Overview → Runtime**. The choice is saved, so an agent restart or reconnect does not return it to auto-detection; if Docker is unusable, this step fails with an error instead of quietly using runveil.

### WSL agent drop-in

```bash
sudo mkdir -p /etc/systemd/system/gpuworks-agent.service.d

sudo tee /etc/systemd/system/gpuworks-agent.service.d/50-wsl-nvidia.conf >/dev/null <<'EOF'
[Service]
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/lib/wsl/lib
Environment=LD_LIBRARY_PATH=/usr/lib/wsl/lib
EOF

sudo systemctl daemon-reload
sudo systemctl restart gpuworks-agent
```

- `50-wsl-nvidia.conf`: so the agent finds WSL-forwarded `nvidia-smi` / CUDA user libraries and does not report `gpu_count=0`.

The node should be **online** with GPU count ≥ 1.

## 5. Prepare the runner image (optional warm-up)

On deploy, the agent pulls the runner selected by the catalog (for example `gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4`; use the tag your console selects).

In mainland China, if direct pulls are unreliable, pull from Huawei Cloud SWR and retag (tag names and versions change between releases; use the one your deployment spec selects):

```bash
docker pull swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:<tag>
docker tag swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:<tag> \
  gpuworks/gpuworks-runner:<tag>
```

Optional CUDA check inside the runner:

```bash
docker run --rm --gpus all --entrypoint nvidia-smi gpuworks/gpuworks-runner:<tag> -L
docker run --rm --gpus all --entrypoint python3 gpuworks/gpuworks-runner:<tag> \
  -c 'import torch; print(torch.cuda.is_available(), torch.cuda.get_device_name(0))'
```

## 6. Deploy a small model and call it

1. In the console, pick a small model template (for example Qwen3-1.7B).
2. Choose an NVIDIA + vLLM spec (for example `bf16-vllm-nvidia-general`). If the GPU matches multiple equally specific specs, you must set the spec name explicitly.
3. Choose a weight source that works on your network (Hugging Face, ModelScope, and so on).
4. Submit the deployment and wait until it is serving (first image pull can take a while; watch free space on the Windows system drive).
5. Create an inference API key and call the OpenAI-compatible API; see [Install GPUWorks and Deploy Your First Model](../tutorials/getting-started.md) and the [OpenAI-Compatible Gateway](../reference/openai-gateway.md):

```bash
curl http://127.0.0.1:8800/v1/chat/completions \
  -H "Authorization: Bearer gw-…" \
  -H "Content-Type: application/json" \
  -d '{"model":"<served_name>","messages":[{"role":"user","content":"hello"}],"max_tokens":64}'
```

Expect HTTP 200 with `choices`.

See [Deploy Models](deploy-models.md) for placement and scheduling details.

## 7. Day-to-day checks

| Check | Command / place | Expect |
|---|---|---|
| WSL distro | `wsl -l -v` | `Running` |
| Control plane | `systemctl is-active gpuworks` | `active` |
| Agent | `systemctl is-active gpuworks-agent` | `active` |
| Docker | `systemctl is-active docker` | `active` |
| Runtime | node details **Overview → Runtime** | `docker` |
| Health | `curl http://127.0.0.1:8800/healthz` | `"status":"ok"` |
| Browser | `http://localhost:8800` | console opens |

## 8. Common issues

| Symptom | What to do |
|---|---|
| Browser cannot open `localhost:8800` | Check whether `wsl -l -v` shows `Stopped`; set `instanceIdleTimeout=-1`, then `wsl --shutdown` and reopen |
| Node `gpu_count=0` | Add `50-wsl-nvidia.conf` and restart the agent |
| Deploy fails with `Failed to infer device type` | The node uses runveil. Switch to Docker as in section 4, pin `docker` in **Overview → Runtime**, then retry the unit |
| Agent log still shows udocker | Act on the reason in parentheses (section 4): with no Docker, `systemctl start docker` and restart the agent; if Docker cannot use the GPU, configure the NVIDIA Container Toolkit and restart Docker |
| Image pull fills the disk or crashes WSL | Free system-drive space first; set Docker's `max-concurrent-downloads` to 2 or 1 as in section 2 |
| Docker Hub / image pull timeouts | Use a reachable mirror, or pre-pull and retag |

## 9. Acceptance checklist

- [ ] `docker run --gpus all … nvidia-smi` succeeds  
- [ ] `gpuworks`, `gpuworks-agent`, and `docker` are `active`  
- [ ] Agent log shows `driver=docker`, and the node runtime is pinned to `docker`  
- [ ] Node is online with GPU count ≥ 1  
- [ ] Small-model deployment is serving  
- [ ] `/v1/chat/completions` returns 200  
- [ ] Windows browser opens `http://localhost:8800`  

If all items pass, the WSL2 + Docker local trial is complete.
