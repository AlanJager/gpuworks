import { useEffect, useState } from "react";
import TemporaryAddressHint from "./TemporaryAddressHint";
import { APIError, apiErrorText } from "../apiError";
import {
  api,
  type CheckResult,
  type ProviderInstallResponse,
  type ProviderInstance,
} from "../api";
import { buildProviderInstallRequest } from "../providerInstallRequest";
import { isCommandChannel } from "../providerConnections";
import { installVerdict, type InstallOutcome } from "../providerInstallOutcome";
import { useTranslation } from "react-i18next";
import { localizedText } from "../i18n/localizedText";
import { DEFAULT_AGENT_LISTEN } from "../agentListen";
import { HelpTip } from "../ui-kit/HelpTip";
import OnboardingChecks, { onboardingCheckText } from "./OnboardingChecks";
import { confirm } from "../ui-kit/feedback";
import { Notice } from "../ui-kit/Notice";
import { PasswordInput } from "../ui-kit/PasswordInput";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "./ui/accordion";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";
import { Input } from "./ui/input";
import { RadioGroup, RadioItem } from "./ui/radio-group";
import { Select, SelectContent, SelectItem, SelectTrigger } from "./ui/select";
import { Switch } from "./ui/switch";
import { Textarea } from "./ui/textarea";

const DIALOG_STYLE = { width: "min(720px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" };
const Label = ({ children }: { children: string }) => <span className="font-semibold text-fg-default">{children}</span>;
const Hint = ({ children }: { children: string }) => <span className="text-fg-muted">{children}</span>;

// AutoDL's port documentation: its Pro instances open HTTP ports only, which
// a server-dialed agent cannot use, so the one-click install never offers it.
export const AUTODL_PORT_DOCS = "https://www.autodl.com/docs/port/";

export default function ProviderInstallModal({
  providerId,
  providerKind,
  defaultServerDialListen,
  serverDialSupported = true,
  serverDialAutoPort = false,
  inst,
  onClose,
  onDone,
}: {
  providerId: number;
  providerKind?: string;
  defaultServerDialListen?: string;
  // The provider's instances can be dialed by the server (the server's
  // provider dictionary says).
  serverDialSupported?: boolean;
  // The server picks the agent's port and address from the provider's port
  // mappings when both are left empty.
  serverDialAutoPort?: boolean;
  inst: ProviderInstance | null;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const language = i18n.resolvedLanguage || i18n.language;
  const [key, setKey] = useState("");
  const [password, setPassword] = useState("");
  const [connectMode, setConnectMode] = useState<"agent_dial" | "server_dial">("agent_dial");
  const [listen, setListen] = useState("");
  const [serverURL, setServerURL] = useState(window.location.origin);
  const [sshConnectionID, setSSHConnectionID] = useState("auto");
  const [serverDialAddress, setServerDialAddress] = useState("");
  const [installDir, setInstallDir] = useState("");
  const [useSystemd, setUseSystemd] = useState(true);

  const [result, setResult] = useState<ProviderInstallResponse | null>(null);
  // The request itself failed (the server answered with an error): nothing
  // came back to show as check rows, so the reason is the whole result.
  const [requestError, setRequestError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const verdict = installVerdict(result, requestError);
  // The agent was put on the instance. Installing again would replace it and
  // create another node, so the form gives way to the result and a Done button.
  const installed = !!verdict && verdict.outcome !== "failed";

  useEffect(() => {
    if (inst) {
      setKey("");
      setPassword("");
      setConnectMode("agent_dial");
      setListen(serverDialAutoPort ? "" : defaultServerDialListen || DEFAULT_AGENT_LISTEN);
      setServerURL(window.location.origin);
      setSSHConnectionID("auto");
      setServerDialAddress(serverDialAutoPort ? "" : inst.public_ip || "");
      setInstallDir("");
      setUseSystemd(true);
      setResult(null);
      setRequestError(null);
    }
  }, [defaultServerDialListen, inst, serverDialAutoPort]);

  const submit = async (allowInstanceRestart = false) => {
    if (!inst) return;
    setSubmitting(true);
    setResult(null);
    setRequestError(null);
    try {
      const next = await api.installAgent(providerId, buildProviderInstallRequest({
        externalID: inst.external_id,
        serverURL: serverURL.trim() || window.location.origin,
        sshKeyPEM: key,
        password,
        sshConnectionID,
        connectMode,
        serverDialListen: listen,
        serverDialAddress,
        allowInstanceRestart,
        installDir,
        noSystemd: !useSystemd,
      }));
      setResult(next);
    } catch (error) {
      if (!allowInstanceRestart && error instanceof APIError && error.code === "INSTANCE_RESTART_REQUIRED") {
        // Opening a port restarts the instance (RunPod): nothing changed yet,
        // so ask, and install again only when the operator agrees.
        setSubmitting(false);
        confirm({
          title: t("install.restartConfirm.title"),
          content: t(restartConfirmContent(error.params?.need)),
          okText: t("install.restartConfirm.ok"),
          okButtonProps: { danger: true },
          cancelText: t("install.cancel"),
          onOk: () => submit(true),
        });
        return;
      }
      setResult(null);
      // A coded refusal (an invalid install directory, say) reads in the
      // operator's language; other failures carry the server's reason.
      setRequestError(apiErrorText(tCommon, error));
    } finally {
      setSubmitting(false);
    }
  };

  // Closing after an install refreshes the instance list, so it shows as onboarded.
  const close = installed ? onDone : onClose;

  const connections = [
    { value: "auto", label: t("install.autoConnection"), disabled: false },
    ...(inst?.ssh_connections || []).map((connection) => ({
      value: connection.id,
      disabled: !connection.install_supported,
      label: isCommandChannel(connection)
        ? localizedText(connection.label, language) || connection.type
        : `${localizedText(connection.label, language) || t(connection.type === "direct" ? "install.direct" : "install.proxy")} · ${connection.user || "root"}@${connection.host}:${connection.port}${connection.install_supported ? "" : t("install.terminalOnly")}`,
    })),
  ];
  const details = [
    ...(result?.precheck_details?.length ? [{ key: "precheck", label: t("install.precheckDetails"), checks: result.precheck_details }] : []),
    ...(result?.install_details?.length ? [{ key: "details", label: t("install.details"), checks: result.install_details }] : []),
  ];

  return (
    <Dialog open={!!inst} onOpenChange={(next) => { if (!next) close(); }}>
      <DialogContent className="provider-install-modal flex max-h-[calc(100dvh-32px)] flex-col" closeButtonLabel={tCommon("actions.close")} style={DIALOG_STYLE}>
        <DialogHeader><DialogTitle>{t("install.title", { name: inst?.name ?? "" })}</DialogTitle></DialogHeader>
        <div className="-mx-6 flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto px-6">
          {submitting && <Notice tone="info" title={t("install.running")} />}
          {verdict && <InstallVerdictBanner outcome={verdict.outcome} detail={verdict.check ? onboardingCheckText(tCommon, verdict.check) : verdict.detail} />}
          {!installed && (
            <>
              <p className="m-0 text-fg-muted">
                {t(autoAuthKey(providerKind))} <HelpTip content={t(`${autoAuthKey(providerKind)}Help`)} />
              </p>
              <Label>{t("install.connection")}</Label>
              <Select value={sshConnectionID} onValueChange={setSSHConnectionID}>
                <SelectTrigger aria-label={t("install.connection")} wrapperClassName="w-full" className="w-full" />
                <SelectContent>
                  {connections.map((connection) => <SelectItem key={connection.value} value={connection.value} disabled={connection.disabled}>{connection.label}</SelectItem>)}
                </SelectContent>
              </Select>
              {serverDialSupported ? (
                <RadioGroup value={connectMode} onValueChange={(mode) => setConnectMode(mode as "agent_dial" | "server_dial")} className="w-auto flex-row flex-wrap">
                  <RadioItem index={0} value="agent_dial" label={t("install.agentDial")} />
                  <RadioItem index={1} value="server_dial" label={t("install.serverDial")} />
                </RadioGroup>
              ) : providerKind === "autodl" ? (
                <p className="m-0 text-fg-muted">
                  {t("install.autodlAgentDialOnly")}{" "}
                  <a href={AUTODL_PORT_DOCS} target="_blank" rel="noreferrer">{t("install.autodlPortDocs")}</a>
                </p>
              ) : null}
              {connectMode === "agent_dial" ? (
                <>
                  <Label>{t("install.serverURL")}</Label>
                  <Input aria-label={t("install.serverURL")} value={serverURL} onChange={(event) => setServerURL(event.target.value)} placeholder={window.location.origin} />
                  {/* What the node will be told to dial: a quick-tunnel address here
                      stops working as soon as the connector restarts. */}
                  <TemporaryAddressHint value={serverURL} />
                </>
              ) : (
                <>
                  {/* Where the agent listens comes first: the address below may
                      name another port when a platform maps it. */}
                  <Label>{t("install.listenAddress")}</Label>
                  <Input aria-label={t("install.listenAddress")} value={listen} onChange={(event) => setListen(event.target.value)} placeholder={serverDialAutoPort ? t("install.automatic") : DEFAULT_AGENT_LISTEN} />
                  <Hint>{t("install.listenAddressHelp")}</Hint>
                  <Label>{t("install.agentAddress")}</Label>
                  <Input
                    aria-label={t("install.agentAddress")}
                    value={serverDialAddress}
                    onChange={(event) => setServerDialAddress(event.target.value)}
                    placeholder={serverDialAutoPort ? t("install.automatic") : t("install.addressPlaceholder")}
                  />
                  <Hint>{t(serverDialAutoPort ? "install.autoPortHelp" : "install.agentAddressHelp")}</Hint>
                </>
              )}
              <Accordion type="single" collapsible>
                <AccordionItem value="advanced">
                  <AccordionTrigger>{t("install.advanced")}</AccordionTrigger>
                  <AccordionContent>
                    <div className="flex w-full flex-col gap-2.5">
                      <Label>{t("install.installDir")}</Label>
                      <Input aria-label={t("install.installDir")} value={installDir} onChange={(event) => setInstallDir(event.target.value)} placeholder="/opt/gpuworks" />
                      <Hint>{t("install.installDirHelp")}</Hint>
                      <Switch checked={useSystemd} onCheckedChange={setUseSystemd} label={t("install.useSystemd")} />
                      <Hint>{t(useSystemd ? "install.useSystemdHelp" : "install.useSystemdOffHelp")}</Hint>
                      <Label>{t("install.credentialOverride")}</Label>
                      <Hint>{t("install.credentialOverrideHelp")}</Hint>
                      <Textarea
                        rows={6}
                        value={key}
                        onChange={(event) => setKey(event.target.value)}
                        placeholder={"-----BEGIN OPENSSH PRIVATE KEY-----\n..."}
                        aria-label={t("install.privateKey")}
                      />
                      <PasswordInput
                        value={password}
                        onChange={(event) => setPassword(event.target.value)}
                        placeholder={t("install.passwordPlaceholder")}
                        aria-label={t("install.password")}
                        autoComplete="off"
                      />
                    </div>
                  </AccordionContent>
                </AccordionItem>
              </Accordion>
            </>
          )}
          {result?.server_url && (
            <span className="text-label text-fg-muted">
              {t("install.serverURLUsed", { url: result.server_url })}
            </span>
          )}
          {result?.ssh_connection && (
            <span className="text-label text-fg-muted">
              {isCommandChannel(result.ssh_connection)
                ? t("install.connectionUsedChannel", { label: localizedText(result.ssh_connection.label, language) || result.ssh_connection.type })
                : t("install.connectionUsed", { label: localizedText(result.ssh_connection.label, language) || "SSH", user: result.ssh_connection.user || "root", host: result.ssh_connection.host, port: result.ssh_connection.port })}
            </span>
          )}
          {result?.ssh_attempts?.filter((attempt) => attempt.status === "failed").map((attempt) => (
            <Notice key={`${attempt.connection_id}:${attempt.target}`} tone="warning" title={t("install.attemptFailed", { target: attempt.target })}>{attempt.detail}</Notice>
          ))}
          {result?.checks && <InstallChecks checks={result.checks} />}
          {details.length > 0 && (
            <Accordion type="multiple">
              {details.map((detail) => (
                <AccordionItem key={detail.key} value={detail.key}>
                  <AccordionTrigger>{detail.label}</AccordionTrigger>
                  <AccordionContent><InstallChecks checks={detail.checks} /></AccordionContent>
                </AccordionItem>
              ))}
            </Accordion>
          )}
        </div>
        <DialogFooter>
          {installed ? (
            <Button variant="primary" onClick={close}>{t("install.done")}</Button>
          ) : (
            <>
              <Button variant="ghost" onClick={close}>{t("install.cancel")}</Button>
              <Button variant="primary" loading={submitting} onClick={() => void submit()}>
                {verdict ? t("install.retry") : t("install.install")}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// The answer to "did it work": a banner at the top of the dialog that stays
// until the dialog is closed, where a toast would have been gone in seconds.
function InstallVerdictBanner({ outcome, detail }: { outcome: InstallOutcome; detail?: string }) {
  const { t } = useTranslation("providers");
  // Zeron alerts have no success tone; a finished install reads as information.
  const tone = outcome === "installed" ? "info" : outcome === "failed" ? "danger" : "warning";
  return (
    <Notice tone={tone} title={t(`install.outcome.${outcome}.title`)}>
      {outcome === "failed" && detail ? detail : t(`install.outcome.${outcome}.description`)}
    </Notice>
  );
}

function InstallChecks({ checks }: { checks: CheckResult[] }) {
  const { t } = useTranslation("providers");
  return <OnboardingChecks checks={checks} labels={{ item: t("install.checks.item"), status: t("install.checks.status"), detail: t("install.checks.detail") }} />;
}

// restartConfirmContent words the restart question by what the restart is
// for: a port, the platform SSH key, or both (INSTANCE_RESTART_REQUIRED's
// need).
function restartConfirmContent(need: unknown): string {
  switch (need) {
    case "ssh_key":
      return "install.restartConfirm.contentSSHKey";
    case "ssh_key_port":
      return "install.restartConfirm.contentSSHKeyPort";
    default:
      return "install.restartConfirm.content";
  }
}

// Each provider gets the platform key onto an instance its own way; the line
// says which, so an operator knows what the server does on their behalf, and
// its help mark (the same key + "Help") holds the details.
function autoAuthKey(providerKind?: string): string {
  switch (providerKind) {
    case "aliyun":
    case "digitalocean":
    case "runpod":
    case "vast":
    case "compshare":
    case "autodl":
      return `install.autoAuth.${providerKind}`;
  }
  return "install.autoAuth.other";
}
