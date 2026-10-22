import { CopyOutlined, ExclamationCircleFilled, InfoCircleFilled } from "@ant-design/icons";
import { Button, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { Node } from "../api";
import { useCopy } from "./CopyButton";
import { runtimePluginInstallCommand } from "./RuntimePluginStatus";

type RuntimeFallbackNode = Pick<Node, "gpu_vendor" | "runtime_fallback_from" | "runtime_fallback_reason" | "runtime_fallback_plugin">;

/**
 * The reasons the agent passes a runtime over for runveil
 * (Node.runtime_fallback_reason). gpu_ready is the passed-over runtime
 * being able to serve again; the agent switches back once the node is idle.
 */
const fallbackCodes = ["gpu_plugin_missing", "gpu_runtime_unconfigured", "gpu_ready"] as const;

function known(code: string): code is (typeof fallbackCodes)[number] {
  return (fallbackCodes as readonly string[]).includes(code);
}

/** The command that sets up the passed-over runtime, or "" when there is none to offer. */
export function runtimeFallbackCommand(node: RuntimeFallbackNode): string {
  const from = node.runtime_fallback_from ?? "";
  switch (node.runtime_fallback_reason) {
    case "gpu_plugin_missing":
      return runtimePluginInstallCommand({ gpu_vendor: node.gpu_vendor, runtime_driver: from });
    case "gpu_runtime_unconfigured":
      if (node.gpu_vendor?.trim().toLowerCase() === "nvidia" && from === "docker") {
        return "sudo nvidia-ctk runtime configure --runtime=docker\nsudo systemctl restart docker";
      }
      return "";
    default:
      return "";
  }
}

/**
 * Why the node runs on runveil although it has Docker (or nerdctl): the
 * agent passed that runtime over because it cannot give containers the
 * cards. Shown beside the runtime selector, with the command that fixes it.
 */
export default function RuntimeFallbackNotice({ node }: { node: RuntimeFallbackNode }) {
  const { t } = useTranslation("nodes");
  const [copy, copyHolder] = useCopy();
  const from = node.runtime_fallback_from ?? "";
  const code = node.runtime_fallback_reason ?? "";
  if (!from || !code) return null;
  const text = known(code)
    ? t(`runtime.fallback.${code}`, { from, plugin: node.runtime_fallback_plugin ?? "" })
    : `${from}: ${code}`;
  const command = runtimeFallbackCommand(node);
  const ready = code === "gpu_ready";
  const install = code === "gpu_plugin_missing";
  const copyCommand = () =>
    copy(command, {
      success: t(install ? "runtime.pluginInstallCopied" : "runtime.configureCopied"),
      manualTitle: t("runtime.manualCopyTitle"),
    });

  return (
    <>
      <Space size={4} align="start" role={ready ? "status" : "alert"}>
        {ready ? <InfoCircleFilled style={{ color: "#1677ff" }} /> : <ExclamationCircleFilled style={{ color: "#fa8c16" }} />}
        <Typography.Text type={ready ? undefined : "warning"} style={ready ? undefined : { color: "#fa8c16" }}>
          {text}
        </Typography.Text>
      </Space>
      {command && (
        <div>
          <Button size="small" icon={<CopyOutlined />} onClick={() => void copyCommand()}>
            {t(install ? "runtime.copyInstallCommand" : "runtime.copyConfigureCommand")}
          </Button>
        </div>
      )}
      {copyHolder}
    </>
  );
}
