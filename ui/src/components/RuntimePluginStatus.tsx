import { CopyOutlined, ExclamationCircleFilled } from "@ant-design/icons";
import { Button, Space, Tooltip, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { Node } from "../api";
import { runtimePluginBlocked } from "../nodes/runtimeGpuBlock";
import { useCopy } from "./CopyButton";

type RuntimePluginNode = Pick<Node, "gpu_vendor" | "runtime_driver" | "runtime_plugin" | "runtime_gpu_plugin" | "runtime_gpu_blocked">;

const NVIDIA_INSTALL = `(
set -eu
N=https://nvidia.github.io/libnvidia-container
U=https://mirrors.ustc.edu.cn/libnvidia-container
PROBE="$N/stable/rpm/x86_64/libnvidia-container1-1.20.0-1.x86_64.rpm"
if curl -LfsS --connect-timeout 3 --max-time 6 -o /dev/null "$PROBE"; then
  SRC="$N"
else
  SRC="$U"
fi
if command -v apt-get >/dev/null 2>&1; then
  PM=apt-get
  K=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  L=/etc/apt/sources.list.d/nvidia-container-toolkit.list
  curl -fsSL "$SRC/gpgkey" | sudo gpg --dearmor --yes -o "$K"
  curl -fsSL "$SRC/stable/deb/nvidia-container-toolkit.list" |
    sed "s#https://nvidia.github.io/libnvidia-container#$SRC#g" |
    sed "s#deb https://#deb [signed-by=$K] https://#g" |
    sudo tee "$L" >/dev/null
  sudo apt-get update
  sudo apt-get install -y nvidia-container-toolkit
else
  PM=""
  for p in dnf yum zypper; do
    command -v "$p" >/dev/null 2>&1 && PM="$p" && break
  done
  [ -n "$PM" ] || { echo 'Unsupported package manager'; false; }
  R=/etc/yum.repos.d
  [ "$PM" = zypper ] && R=/etc/zypp/repos.d
  curl -fsSL "$SRC/stable/rpm/nvidia-container-toolkit.repo" |
    sed "s#https://nvidia.github.io/libnvidia-container#$SRC#g" |
    sudo tee "$R/nvidia-container-toolkit.repo" >/dev/null
  if [ "$PM" = zypper ]; then
    sudo zypper --gpg-auto-import-keys install -y nvidia-container-toolkit
  else
    sudo "$PM" install -y nvidia-container-toolkit
  fi
fi
)`;

export function runtimePluginInstallCommand(node: RuntimePluginNode): string {
  const vendor = node.gpu_vendor?.trim().toLowerCase();
  const runtime = node.runtime_driver?.trim().toLowerCase();
  if (vendor === "nvidia" && (runtime === "docker" || runtime === "nerdctl")) {
    if (runtime === "docker") {
      return `${NVIDIA_INSTALL}\nstatus=$?\nif [ "$status" = 0 ]; then\n  sudo nvidia-ctk runtime configure --runtime=docker\n  sudo systemctl restart docker\nfi`;
    }
    return NVIDIA_INSTALL;
  }
  if (vendor === "ascend" && runtime === "docker") {
    return `set -eu
installer="$(find . -maxdepth 1 -type f -name 'Ascend-docker-runtime_*_linux-*.run' -print -quit)"
test -n "$installer" || { echo 'Place the downloaded Ascend Docker Runtime .run package in the current directory' >&2; exit 1; }
chmod +x "$installer"
sudo "$installer" --install
sudo systemctl restart docker`;
  }
  return "";
}

/** The server's verdict: the node is cordoned because its runtime lacks the vendor plugin. */
export function runtimePluginMissing(node: RuntimePluginNode): boolean {
  return runtimePluginBlocked(node);
}

export default function RuntimePluginStatus({ node }: { node: RuntimePluginNode }) {
  const { t } = useTranslation("nodes");
  const [copy, copyHolder] = useCopy();
  const command = runtimePluginInstallCommand(node);

  if (!runtimePluginMissing(node)) return <>{node.runtime_plugin || "—"}</>;
  const pluginName = node.runtime_gpu_plugin || node.runtime_plugin || "";

  const copyInstallCommand = () => copy(command, { success: t("runtime.pluginInstallCopied"), manualTitle: t("runtime.manualCopyTitle") });

  return (
    <>
      <Space wrap>
        <Tooltip title={t("runtime.pluginMissingHint")}>
          <Space size={4} role="alert">
            <ExclamationCircleFilled style={{ color: "#faad14" }} />
            <Typography.Text type="warning">{pluginName ? `${pluginName} · ` : ""}{t("runtime.pluginNotInstalled")}</Typography.Text>
          </Space>
        </Tooltip>
        {command && (
          <Button size="small" icon={<CopyOutlined />} onClick={() => void copyInstallCommand()}>
            {t("runtime.copyInstallCommand")}
          </Button>
        )}
      </Space>
      {copyHolder}
    </>
  );
}
