package main

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/testenv/goldenimage"
)

// The source VM boots Alibaba Cloud's public "Ubuntu 22.04 with NVIDIA GPU
// driver and CUDA" image, so the driver is the cloud's own and matches what an
// operator's GPU instance carries. Docker, the NVIDIA container toolkit, the
// vLLM runner image and the model are added on top and baked into the golden
// image; L4 cases then neither install nor download anything.
//
// Every download goes to a mainland mirror first. The builder runs as root.

// dockerMajor is the Docker CE series the L4 cases pin (the CompShare cases
// pin the same one), so a failure is never a Docker upgrade in disguise.
const dockerMajor = "29"

// dockerMirrors are the Docker CE apt repositories tried in order, all
// mainland: one caught mid-sync serves an index whose size does not match its
// Release file, and apt refuses it until the sync ends.
const dockerMirrors = "https://mirrors.aliyun.com/docker-ce/linux/ubuntu https://mirrors.tuna.tsinghua.edu.cn/docker-ce/linux/ubuntu https://mirrors.ustc.edu.cn/docker-ce/linux/ubuntu https://repo.huaweicloud.com/docker-ce/linux/ubuntu"

// installDockerCommand installs Docker CE <dockerMajor>.x and the NVIDIA
// container toolkit, configures the nvidia runtime, and prints one evidence
// line the validation (and the L4 cases) check.
func installDockerCommand() string {
	return `set -eu
export DEBIAN_FRONTEND=noninteractive
apt_retry() {
  attempt=1
  while [ "$attempt" -le 8 ]; do
    if "$@"; then return 0; fi
    attempt=$((attempt + 1))
    sleep 10
  done
  return 1
}
. /etc/os-release
case "${ID:-}" in ubuntu) ;; *) echo "unsupported distribution: ${ID:-unknown}" >&2; exit 1;; esac
codename="${VERSION_CODENAME:?}"
arch="$(dpkg --print-architecture)"
# A resumed build finds the Docker repository an earlier attempt wrote; a
# mirror of it caught mid-sync would fail the base update too.
rm -f /etc/apt/sources.list.d/docker.list
apt_retry apt-get update -o Acquire::Retries=3
apt_retry apt-get install -y --no-upgrade curl gnupg ca-certificates
install -m 0755 -d /etc/apt/keyrings
docker_version=""
for base in ` + dockerMirrors + `; do
  if curl -fsSL --retry 3 --connect-timeout 15 "$base/gpg" -o /etc/apt/keyrings/docker.asc; then
    chmod a+r /etc/apt/keyrings/docker.asc
    printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] %s %s stable\n' "$arch" "$base" "$codename" >/etc/apt/sources.list.d/docker.list
    # A mirror caught mid-sync fails the index check; the next one is tried.
    for try in 1 2 3; do
      if apt-get update -o Acquire::Retries=3; then
        docker_version="$(apt-cache madison docker-ce | awk '$3 ~ /^5:` + dockerMajor + `\./ {print $3; exit}')"
        break
      fi
      sleep 10
    done
  fi
  if [ -n "$docker_version" ]; then echo "docker-ce installed from $base"; break; fi
done
[ -n "$docker_version" ] || { echo 'Docker CE ` + dockerMajor + `.x is unavailable from every mirror' >&2; exit 1; }
apt_retry apt-get install -y --allow-downgrades docker-ce="$docker_version" docker-ce-cli="$docker_version" containerd.io docker-buildx-plugin docker-compose-plugin
` + toolkitInstall + `
nvidia-ctk runtime configure --runtime=docker
systemctl enable docker
systemctl restart docker
server_version="$(docker version --format '{{.Server.Version}}')"
case "$server_version" in ` + dockerMajor + `.*) ;; *) echo "unexpected Docker server version $server_version" >&2; exit 1;; esac
docker info --format '{{json .Runtimes}}' | grep -q nvidia
printf 'GPUWORKS_DOCKER_INSTALL_EVIDENCE docker=%s nvidia_ctk=%s nvidia_runtime=yes\n' "$server_version" "$(nvidia-ctk --version | head -n1 | tr ' ' '_')"`
}

// toolkitInstall installs the NVIDIA container toolkit unless the image has
// it. The apt repository is tried at each mirror in order; the first whose
// package index loads wins.
const toolkitInstall = `if ! command -v nvidia-ctk >/dev/null 2>&1; then
  installed=""
  for base in https://mirrors.aliyun.com/nvidia-container-toolkit https://mirrors.ustc.edu.cn/libnvidia-container https://nvidia.github.io/libnvidia-container; do
    if curl -fsSL --retry 2 --connect-timeout 15 "$base/gpgkey" | gpg --dearmor --yes -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg 2>/dev/null \
      && curl -fsSL --retry 2 --connect-timeout 15 "$base/stable/deb/nvidia-container-toolkit.list" | sed "s#https://nvidia.github.io/libnvidia-container#$base#g; s#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g" >/etc/apt/sources.list.d/nvidia-container-toolkit.list \
      && apt-get update -o Acquire::Retries=2 >/dev/null 2>&1 \
      && apt-get install -y nvidia-container-toolkit >/dev/null 2>&1; then
      installed="$base"; break
    fi
  done
  [ -n "$installed" ] || { echo 'nvidia-container-toolkit is unavailable from every mirror' >&2; exit 1; }
  echo "nvidia-container-toolkit installed from $installed"
fi`

// pullImageCommand pulls the runner image from the first source that works
// and tags it as image, the name the cases deploy: Docker Hub is not
// dependable from mainland instances, so mirrors come first.
func pullImageCommand(image string, sources []string) string {
	var attempts strings.Builder
	for _, source := range sources {
		fmt.Fprintf(&attempts, "  for try in 1 2 3; do if docker pull %[1]s; then ok=%[1]s; break 2; fi; sleep 10; done\n", goldenimage.ShellQuote(source))
	}
	return fmt.Sprintf(`set -eu
ok=""
while :; do
%s  break
done
[ -n "$ok" ] || { echo 'the runner image is unavailable from every source' >&2; exit 1; }
if [ "$ok" != %[2]s ]; then docker tag "$ok" %[2]s; fi
docker image inspect %[2]s --format 'GPUWORKS_RUNNER_IMAGE {{.Id}} {{.Size}}'`, attempts.String(), goldenimage.ShellQuote(image))
}

// finishCommand quiesces the VM for the snapshot: Docker stopped (no layer
// writes in flight), caches and shell history dropped, filesystem synced. It
// leaves cloud-init and the SSH host keys alone: Alibaba Cloud injects the
// password and host name of every new instance through cloud-init.
func finishCommand() string {
	return `set -eu
systemctl stop docker.socket docker containerd || true
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* /root/.cache /root/.bash_history
journalctl --rotate >/dev/null 2>&1 || true
journalctl --vacuum-time=1s >/dev/null 2>&1 || true
find /var/log -type f -name '*.log' -exec truncate -s 0 {} + 2>/dev/null || true
sync`
}

// bootedChecksCommand verifies a VM booted from the golden image: the model,
// the runner image, Docker 29 with the nvidia runtime, the driver, and a
// container that sees the GPU. It prints the evidence lines the L4 cases
// expect.
func bootedChecksCommand(modelPath, image string) string {
	return "set -eu\n" + goldenimage.ValidationCommand(modelPath, image, "") + `
systemctl is-active docker >/dev/null
server_version="$(docker version --format '{{.Server.Version}}')"
case "$server_version" in ` + dockerMajor + `.*) ;; *) echo "unexpected Docker server version $server_version" >&2; exit 1;; esac
docker info --format '{{json .Runtimes}}' | grep -q nvidia
nvidia-smi -L
docker run --rm --gpus all --entrypoint nvidia-smi ` + goldenimage.ShellQuote(image) + ` -L
printf 'GPUWORKS_GOLDEN_OK docker=%s\n' "$server_version"`
}
