import { ExclamationCircleFilled, InfoCircleFilled } from "@ant-design/icons";
import { Button } from "./ui/button";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { useTranslation } from "react-i18next";
import type { Node, RuntimeSocketDenial } from "../api";
import CopyButton, { useCopy } from "./CopyButton";
import { Notice } from "../ui-kit/Notice";
import { runtimePluginInstallCommand } from "./RuntimePluginStatus";

type RuntimeFallbackNode = Pick<Node, "gpu_vendor" | "runtime_fallback_from" | "runtime_fallback_reason" | "runtime_fallback_plugin" | "runtime_fallback_socket">;

/**
 * The reasons the agent passes a runtime over for runveil
 * (Node.runtime_fallback_reason). daemon_not_ready is a daemon that did not
 * answer while the agent started; socket_permission_denied is a socket the
 * agent's account may not open, which needs the agent restarted once fixed;
 * gpu_ready is the passed-over runtime being able to serve again; the agent
 * switches back once the node is idle.
 */
const fallbackCodes = ["gpu_plugin_missing", "gpu_runtime_unconfigured", "daemon_not_ready", "socket_permission_denied", "gpu_ready"] as const;

function known(code: string): code is (typeof fallbackCodes)[number] {
  return (fallbackCodes as readonly string[]).includes(code);
}

/** The command that sets up the passed-over runtime, or "" when there is none to offer. */
export function runtimeFallbackCommand(node: RuntimeFallbackNode): string {
  const from = node.runtime_fallback_from ?? "";
  switch (node.runtime_fallback_reason) {
    case "gpu_plugin_missing":
      return runtimePluginInstallCommand({ gpu_vendor: node.gpu_vendor, runtime_driver: from });
    case "socket_permission_denied":
      // The server builds it from what the agent found (account, group, how it restarts).
      return node.runtime_fallback_socket?.command ?? "";
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
  const socket = code === "socket_permission_denied" ? node.runtime_fallback_socket : undefined;
  if (socket && Object.prototype.hasOwnProperty.call(socketFix, socket.cause)) {
    return <SocketDenialNotice from={from} socket={socket} command={runtimeFallbackCommand(node)} />;
  }
  const text = socket
    ? `${from}: ${code} (${socket.cause})`
    : known(code) && code !== "socket_permission_denied"
      ? t(`runtime.fallback.${code}`, { from, plugin: node.runtime_fallback_plugin ?? "" })
      : `${from}: ${code}`;
  const command = runtimeFallbackCommand(node);
  const ready = code === "gpu_ready";
  const install = code === "gpu_plugin_missing";
  const copyCommand = () =>
    copy(command, socket
      ? { success: t("runtime.socketCommandCopied"), manualTitle: t("runtime.socketCommandTitle") }
      : {
          success: t(install ? "runtime.pluginInstallCopied" : "runtime.configureCopied"),
          manualTitle: t("runtime.manualCopyTitle"),
        });

  return (
    <>
      <div className="flex items-start gap-1" role={ready ? "status" : "alert"}>
        {ready ? <InfoCircleFilled className="mt-[3px] text-fg-info" /> : <ExclamationCircleFilled className="mt-[3px] text-fg-warning" />}
        <span className={ready ? undefined : "text-fg-warning"}>
          {text}
        </span>
      </div>
      {command && (
        <div>
          <Button size="xs" variant="tertiary" leadingIcon={ConsoleIcon.copy} onClick={() => void copyCommand()}>
            {t(socket ? "runtime.copySocketCommand" : install ? "runtime.copyInstallCommand" : "runtime.copyConfigureCommand")}
          </Button>
        </div>
      )}
      {copyHolder}
    </>
  );
}

/**
 * The fix sentence for each cause the server reads from the agent's facts,
 * or none when there is nothing to do about it here. A cause this client
 * does not know is shown as its code.
 */
const socketFix = {
  not_in_group: "runtime.fallback.socket.fix.not_in_group",
  restart_pending: "runtime.fallback.socket.fix.restart_pending",
  group_held: "runtime.fallback.socket.fix.group_held",
  root_group: "runtime.fallback.socket.fix.root_group",
  unknown: undefined,
} as const;

/** Causes whose way out is restarting the agent. */
const socketRestartCauses = new Set(["not_in_group", "restart_pending", "root_group"]);

/**
 * How to restart an agent whose supervisor is not known, so the server's
 * command could not include it: through its unit, or with the script a
 * nohup install keeps in its state directory.
 */
const agentRestartWays = [
  { way: "systemd", command: "sudo systemctl restart gpuworks-agent" },
  { way: "manual", command: "./agent-restart.sh" },
] as const;

/**
 * The notice for a socket the agent's account may not open, from what the
 * server sent, one part per line: why (its cause), that the node runs on
 * runveil meanwhile, a directory-service group of the same name when there
 * is one, what to do with the server's command, how to restart the agent
 * when that command could not include it, what a restart does to the node's
 * instances, and how to stop the notice when the account is not meant to
 * use the runtime.
 */
function SocketDenialNotice({ from, socket, command }: { from: string; socket: RuntimeSocketDenial; command: string }) {
  const { t } = useTranslation("nodes");
  const [copy, copyHolder] = useCopy();
  const cause = socket.cause as keyof typeof socketFix;
  const account = socket.account || t("runtime.fallback.socket.accountUnknown");
  const group = socket.group
    ? t("runtime.fallback.socket.group", { group: socket.group, gid: socket.gid })
    : t("runtime.fallback.socket.gid", { gid: socket.gid });
  const fix = socketFix[cause];
  const restarts = socketRestartCauses.has(cause);
  const sameName = Boolean(socket.same_name_gid && socket.group && (cause === "not_in_group" || cause === "restart_pending"));
  return (
    <Notice tone="warning" role="alert" title={<span data-part="summary">{t(`runtime.fallback.socket.summary.${cause}`, { from, account, group, socket: socket.socket })}</span>}>
      <div className="flex flex-col gap-1">
        <p className="m-0" data-part="impact">{t("runtime.fallback.socket.impact")}</p>
        {sameName && (
          <p className="m-0" data-part="sameName">
            {t("runtime.fallback.socket.sameName", { account, name: socket.group, sameGid: socket.same_name_gid, gid: socket.gid })}
          </p>
        )}
        {fix && <p className="m-0" data-part="fix">{t(fix, { account })}</p>}
        {command && (
          <div>
            <Button
              size="xs"
              variant="tertiary"
              leadingIcon={ConsoleIcon.copy}
              onClick={() => void copy(command, { success: t("runtime.socketCommandCopied"), manualTitle: t("runtime.socketCommandTitle") })}
            >
              {t("runtime.copySocketCommand")}
            </Button>
          </div>
        )}
        {restarts && !socket.supervisor && (
          <ul className="m-0 flex list-none flex-col gap-1 p-0">
            {agentRestartWays.map(({ way, command: restart }) => {
              const label = t(`runtime.fallback.socket.restart.${way}`, { account });
              return (
                <li key={way} data-part="restart">
                  <p className="m-0">{label}</p>
                  <div className="flex items-start gap-1">
                    <pre tabIndex={0} aria-label={label} className="m-0 flex-1 overflow-x-auto px-3 py-2">{restart}</pre>
                    <CopyButton
                      text={restart}
                      label={t("runtime.fallback.socket.restart.copy")}
                      success={t("runtime.socketCommandCopied")}
                      manualTitle={t("runtime.socketCommandTitle")}
                      size="small"
                    />
                  </div>
                </li>
              );
            })}
          </ul>
        )}
        {restarts && <p className="m-0" data-part="after">{t("runtime.fallback.socket.after", { from })}</p>}
        <p className="m-0" data-part="optOut">{t("runtime.fallback.socket.optOut", { account, from })}</p>
      </div>
      {copyHolder}
    </Notice>
  );
}
