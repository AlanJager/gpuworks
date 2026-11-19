import { useEffect, useRef, useState, type ReactNode } from "react";
import { useForm } from "react-hook-form";
import { api, type CheckResult, type SSHInstallReport, type SSHPrecheckReport } from "../api";
import { APIError, apiErrorText } from "../apiError";
import { buildInstallCommand } from "../nodeInstallCommand";
import { DEFAULT_AGENT_LISTEN } from "../agentListen";
import SSHCredentialFields, { type SSHAuthMode, type SSHSecretValues } from "./SSHCredentialFields";
import TemporaryAddressHint from "./TemporaryAddressHint";
import { useCopy } from "./CopyButton";
import OnboardingChecks, { countOnboardingChecks, type OnboardingCheckCounts } from "./OnboardingChecks";
import { useTranslation } from "react-i18next";
import { confirm, message } from "../ui-kit/feedback";
import { FormField } from "../ui-kit/form/FormField";
import { HelpTip } from "../ui-kit/HelpTip";
import { Notice } from "../ui-kit/Notice";
import { SideDrawer } from "../ui-kit/SideDrawer";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "./ui/accordion";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Field, FieldDescription, FieldLabel } from "./ui/field";
import { Input } from "./ui/input";
import { RadioGroup, RadioItem } from "./ui/radio-group";
import { Switch } from "./ui/switch";

type SSHFormValues = SSHSecretValues & { target: string };
const SSH_BLANK: SSHFormValues = { target: "", password: undefined, private_key_pem: undefined };

// Install answers these when the host already has a gpuworks agent.
const REPLACE_AGENT_CODES = new Set(["NODE_INSTALL_HOST_ALREADY_NODE", "NODE_INSTALL_EXISTING_AGENT", "NODE_INSTALL_EXISTING_AGENT_UNKNOWN", "NODE_INSTALL_EXISTING_AGENT_SELF", "NODE_INSTALL_EXISTING_AGENT_SAME_DIR"]);

// The offline package a node the server dials installs from when it has no
// SSH: it cannot download from the server (offlinepkg on the server).
const OFFLINE_PACKAGE = "/dl/gpuworks-agent.run";

export default function AddNodeDrawer({
  open,
  onClose,
  onInstalled,
}: {
  open: boolean;
  onClose: () => void;
  onInstalled: () => void;
}) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const [copy, copyHolder] = useCopy();
  const origin = window.location.origin;
  const form = useForm<SSHFormValues>({ shouldFocusError: false, defaultValues: SSH_BLANK });
  const [sshSupported, setSshSupported] = useState<"yes" | "no">("yes");
  const [connectMode, setConnectMode] = useState<"agent_dial" | "server_dial">("agent_dial");
  const [serverURL, setServerURL] = useState(origin);
  // A node the server dials never contacts the server, so it is given no
  // server address at all.
  const effectiveServerURL = connectMode === "agent_dial" ? serverURL.trim() || origin : undefined;
  const [hostAddress, setHostAddress] = useState("");
  const [installDir, setInstallDir] = useState("");
  const [serverDialListen, setServerDialListen] = useState(DEFAULT_AGENT_LISTEN);
  const [offlineChecksum, setOfflineChecksum] = useState("");
  const [useSystemd, setUseSystemd] = useState(true);
  const [nodeName, setNodeName] = useState("");
  const [pendingCommand, setPendingCommand] = useState("");
  const [pendingNodeUUID, setPendingNodeUUID] = useState("");
  const requestVersion = useRef(0);
  const [creatingPending, setCreatingPending] = useState(false);
  const [authMode, setAuthMode] = useState<SSHAuthMode>("platform_key");
  const [report, setReport] = useState<SSHPrecheckReport | null>(null);
  const [installReport, setInstallReport] = useState<SSHInstallReport | null>(null);
  const [checking, setChecking] = useState(false);
  const [installing, setInstalling] = useState(false);
  const installInFlight = useRef(false);
  const precheckSummary = countOnboardingChecks(report?.checks ?? []);
  const installSummary = countOnboardingChecks(installReport?.checks ?? []);

  const cmd = pendingCommand || t("add.commandPlaceholder");

  const resetResults = () => {
    requestVersion.current++;
    setPendingNodeUUID("");
    setPendingCommand("");
    setChecking(false);
    setCreatingPending(false);
    setReport(null);
    setInstallReport(null);
  };

  const resetForm = () => {
    form.reset(SSH_BLANK);
    resetResults();
    setSshSupported("yes");
    setConnectMode("agent_dial");
    setServerURL(origin);
    setHostAddress("");
    setInstallDir("");
    setServerDialListen(DEFAULT_AGENT_LISTEN);
    setUseSystemd(true);
    setNodeName("");
    setPendingCommand("");
    setAuthMode("platform_key");
  };

  const handleClose = () => { resetForm(); onClose(); };

  // Any edit of the SSH fields makes the precheck and install results stale.
  useEffect(() => {
    const subscription = form.watch((_, { type }) => { if (type === "change") resetResults(); });
    return () => subscription.unsubscribe();
  }, [form]);

  const previouslyOpen = useRef(open);
  useEffect(() => {
    // A never-opened drawer has no mounted Form to reset. Initial state is
    // already empty; reset only when an open drawer is closed externally.
    if (previouslyOpen.current && !open) resetForm();
    previouslyOpen.current = open;
  }, [open]);

  useEffect(() => {
    if (!open || !pendingNodeUUID) return;
    const version = requestVersion.current;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const checkConnection = async () => {
      try {
        const node = await api.node(pendingNodeUUID);
        if (!active || version !== requestVersion.current) return;
        if (node.uuid === pendingNodeUUID && node.state === "online") {
          message.success(t("add.message.connected"));
          onInstalled();
          handleClose();
          return;
        }
      } catch {
        // Installation can complete before registration; a transient read
        // failure must not report success or discard the installation report.
      }
      if (active && version === requestVersion.current) timer = setTimeout(checkConnection, 3000);
    };
    void checkConnection();
    return () => { active = false; clearTimeout(timer); };
  }, [open, pendingNodeUUID]);

  const createPendingNode = async () => {
    if (connectMode === "server_dial" && !hostAddress.trim()) {
      message.error(t("add.message.hostAddressRequired"));
      return
    }
    setCreatingPending(true);
    const version = requestVersion.current;
    try {
      const created = await api.createNode({
        name: nodeName.trim() || undefined,
        connect_mode: connectMode,
        server_dial_listen: connectMode === "server_dial" ? serverDialListen.trim() || DEFAULT_AGENT_LISTEN : undefined,
        host_address: connectMode === "server_dial" ? hostAddress || undefined : undefined,
        server_url: effectiveServerURL,
      });
      if (version !== requestVersion.current) return;
      setPendingNodeUUID(created.node.uuid);
      setPendingCommand(buildInstallCommand({
        baseCommand: created.install_command,
        token: created.token,
        connectMode,
        serverURL: effectiveServerURL ?? "",
        origin,
        installDir,
        noSystemd: !useSystemd,
      }));
      message.success(t("add.message.created"));
      onInstalled();
    } catch (e) {
      if (version !== requestVersion.current) return;
      message.error(t("add.message.createFailed", { detail: (e as Error).message }));
    } finally {
      if (version === requestVersion.current) setCreatingPending(false);
    }
  };

  const copyCommand = () => copy(cmd, { success: t("add.message.copied") });

  const buildAuthPayload = (values: SSHFormValues) => ({
    target: values.target,
    name: nodeName.trim() || undefined,
    auth_mode: authMode,
    password: authMode === "password" || authMode === "keyboard_interactive" ? values.password : undefined,
    private_key_pem: authMode === "private_key" ? values.private_key_pem?.trim() || undefined : undefined,
    server_url: effectiveServerURL,
    install_dir: installDir || undefined,
    connect_mode: connectMode,
    server_dial_listen: connectMode === "server_dial" ? serverDialListen.trim() || DEFAULT_AGENT_LISTEN : undefined,
    host_address: connectMode === "server_dial" ? hostAddress.trim() : undefined,
    no_systemd: useSystemd ? undefined : true,
  });

  // The package's checksum, shown beside its download once there is a
  // command to run it with.
  useEffect(() => {
    if (!pendingCommand || connectMode !== "server_dial" || sshSupported !== "no" || typeof fetch !== "function") return;
    let active = true;
    fetch(`${OFFLINE_PACKAGE}.sha256`)
      .then((res) => (res.ok ? res.text() : ""))
      .then((text) => { if (active) setOfflineChecksum(text.trim().split(/\s+/)[0] ?? ""); })
      .catch(() => undefined);
    return () => { active = false; };
  }, [pendingCommand, connectMode, sshSupported]);

  const requireHostAddress = () => {
    if (connectMode === "server_dial" && !hostAddress.trim()) {
      message.error(t("add.message.hostAddressRequired"));
      return false;
    }
    return true;
  };

  const runPrecheck = async () => {
    const version = requestVersion.current;
    const values = (await form.trigger()) ? form.getValues() : null;
    if (!values || version !== requestVersion.current) return; // the form shows the field errors
    if (!requireHostAddress()) return;
    setChecking(true);
    try {
      const next = await api.precheckNode(buildAuthPayload(values));
      if (version !== requestVersion.current) return;
      setReport(next);
      setInstallReport(null);
      const summary = countOnboardingChecks(next.checks ?? []);
      if (summary.fail > 0) {
        message.error(t("add.message.precheckWithErrors"));
      } else if (summary.warn > 0 || summary.skip > 0) {
        message.warning(t("add.message.precheckWithIssues"));
      } else {
        message.success(t("add.message.precheckComplete"));
      }
    } catch (e) {
      if (version !== requestVersion.current) return;
      message.error(t("add.message.precheckFailed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      if (version === requestVersion.current) setChecking(false);
    }
  };

  const runInstall = async (replaceExistingAgent = false) => {
    if (installInFlight.current) return;
    const version = requestVersion.current;
    const values = (await form.trigger()) ? form.getValues() : null;
    if (!values || version !== requestVersion.current || installInFlight.current) return;
    if (!requireHostAddress()) return;
    // Invalidating form results does not cancel the remote operation. Keep
    // this guard until it settles, including across drawer close/reopen.
    installInFlight.current = true;
    setInstalling(true);
    try {
      const next = await api.installNode({ ...buildAuthPayload(values), replace_existing_agent: replaceExistingAgent || undefined });
      if (version !== requestVersion.current) return;
      setInstallReport(next);
      // Failed installation rejects the request. A successful report may
      // still contain runtime capability errors that do not prevent joining.
      setPendingNodeUUID(next.node_uuid);
      message.success(t("add.message.installExecuted"));
      onInstalled();
    } catch (e) {
      if (version !== requestVersion.current) return;
      if (!replaceExistingAgent && e instanceof APIError && REPLACE_AGENT_CODES.has(e.code)) {
        // The host already has an agent: installing replaces it, so ask first.
        confirm({
          title: t("add.replaceAgent.title"),
          // A nohup install replaces only the agent in its own directory,
          // which the error already says; the others speak of the service.
          content: e.code === "NODE_INSTALL_EXISTING_AGENT_SAME_DIR" ? apiErrorText(tCommon, e) : `${apiErrorText(tCommon, e)} ${t("add.replaceAgent.content")}`,
          okText: t("add.replaceAgent.confirm"),
          okButtonProps: { danger: true },
          cancelText: t("add.replaceAgent.cancel"),
          onOk: () => version === requestVersion.current ? runInstall(true) : undefined,
        });
      } else {
        message.error(t("add.message.installFailed", { detail: apiErrorText(tCommon, e) }));
      }
    } finally {
      installInFlight.current = false;
      setInstalling(false);
    }
  };

  // Install directory and systemd apply whether the agent is installed over
  // SSH or by hand, so both branches render the same advanced section.
  const advanced = (
    <Accordion type="single" collapsible>
      <AccordionItem value="advanced">
        <AccordionTrigger>{t("add.advanced")}</AccordionTrigger>
        <AccordionContent>
          <div className="flex flex-col gap-3">
            <Field>
              <span className="inline-flex items-center gap-1"><FieldLabel>{t("add.installDir")}</FieldLabel><HelpTip content={t("add.installDirTooltip")} /></span>
              <Input value={installDir} onChange={(e) => { resetResults(); setInstallDir(e.target.value); }} placeholder={t("add.installDirPlaceholder")} />
              <FieldDescription>{t("add.installDirHelp")}</FieldDescription>
            </Field>
            <div className="flex flex-col gap-1.5">
              <span className="inline-flex items-center gap-1">
                <Switch checked={useSystemd} onCheckedChange={(checked) => { resetResults(); setUseSystemd(checked); }} label={t("add.useSystemd")} />
                <HelpTip content={t("add.useSystemdHelp")} />
              </span>
              {!useSystemd && <span className="px-1.5 text-label text-fg-subtle">{t("add.useSystemdOff")}</span>}
            </div>
          </div>
        </AccordionContent>
      </AccordionItem>
    </Accordion>
  );

  return (
    <SideDrawer title={t("action.add")} open={open} onClose={handleClose} width={640}>
      {copyHolder}
      <div className="flex w-full flex-col gap-4">
        {pendingNodeUUID && <Notice tone="info" title={t("add.waitingForConnection")}>{t("add.waitingForConnectionHelp")}</Notice>}
        <Choice label={t("add.sshSupportLabel")} value={sshSupported}
          onChange={(value) => { resetResults(); setSshSupported(value as "yes" | "no"); }}
          options={[{ value: "yes", label: t("add.sshSupported") }, { value: "no", label: t("add.sshUnsupported") }]} />

        <Field>
          <FieldLabel>{t("add.nodeNameOptional")}</FieldLabel>
          <Input value={nodeName} onChange={(e) => { resetResults(); setNodeName(e.target.value); }} placeholder={t("add.nodeNamePlaceholder")} />
        </Field>

        <Choice label={t("add.connectionDirection")} value={connectMode}
          onChange={(value) => { resetResults(); setConnectMode(value as "agent_dial" | "server_dial"); }}
          options={[{ value: "agent_dial", label: t("connection.agentDial") }, { value: "server_dial", label: t("connection.serverDial") }]} />

        {connectMode === "agent_dial" ? (
          <Field>
            <FieldLabel>{t("detail.serverExternalAddress")}</FieldLabel>
            <Input value={serverURL} onChange={(e) => { resetResults(); setServerURL(e.target.value); }} placeholder={origin} />
            {/* The hint belongs on the address the node will dial: it becomes
                server_url and has to keep working after the connector restarts. */}
            <TemporaryAddressHint value={serverURL} />
          </Field>
        ) : (
          <>
            {/* Where the agent listens comes first: the address below may name
                another port when a platform maps it, and reading only that
                one the agent would seem to listen there. */}
            <Field>
              <FieldLabel>{t("add.agentListenAddress")}</FieldLabel>
              <Input
                aria-label={t("add.agentListenAddress")}
                value={serverDialListen}
                onChange={(e) => { resetResults(); setServerDialListen(e.target.value); }}
                placeholder={DEFAULT_AGENT_LISTEN}
              />
              <FieldDescription>{t("add.agentListenAddressHelp")}</FieldDescription>
            </Field>
            <Field>
              <FieldLabel>{t("add.hostAddress")}<span aria-hidden="true"> *</span></FieldLabel>
              <Input
                aria-label={t("add.hostAddress")}
                aria-required
                value={hostAddress}
                onChange={(e) => { resetResults(); setHostAddress(e.target.value); }}
                placeholder={t("add.hostAddressPlaceholder")}
              />
              <FieldDescription>{t("add.hostAddressHelp")}</FieldDescription>
            </Field>
          </>
        )}

        {sshSupported === "yes" ? (
          <>
            <form noValidate onSubmit={(event) => event.preventDefault()} className="flex flex-col gap-4">
              <FormField control={form.control} name="target" required label={t("add.target")} description={t("add.targetHelp")}
                rules={{ validate: (value) => (value ?? "").trim() !== "" || t("add.targetRequired") }}>
                {(binding) => <Input {...binding} placeholder="root@192.0.2.10:22" />}
              </FormField>
              <SSHCredentialFields control={form.control} authMode={authMode} onAuthModeChange={(mode) => { resetResults(); setAuthMode(mode); }} />

              {advanced}

              <div className="flex flex-wrap items-center gap-2">
                <Button type="button" variant="primary" loading={checking} disabled={installing || !!pendingNodeUUID} onClick={() => void runPrecheck()}>
                  {t("add.startPrecheck")}
                </Button>
                <Button type="button" variant="tertiary" loading={installing} disabled={installing || checking || !!pendingNodeUUID} onClick={() => void runInstall()}>
                  {t("add.installAgent")}
                </Button>
                {report?.host && (
                  <span className="text-fg-muted">
                    {report.user}@{report.host}:{report.port}
                  </span>
                )}
              </div>
            </form>
            {report && <SummaryStrip title={t("add.precheckSummary")} summary={precheckSummary} tone="info" />}
            {report && <ReportTable title={t("add.precheckResults")} checks={report.checks} />}
            {installReport && (
              <>
                <SummaryStrip
                  title={t("add.installSummary")}
                  summary={installSummary}
                  tone={installSummary.fail > 0 ? "danger" : installSummary.warn > 0 || installSummary.skip > 0 ? "warning" : "info"}
                />
                <ReportTable
                  title={t("add.installResults", { binary: installReport.binary_name, installDir: installReport.install_dir, address: installReport.server_dial_address ? ` · ${installReport.server_dial_address}` : "" })}
                  checks={installReport.checks}
                />
              </>
            )}
          </>
        ) : (
          <>
            {advanced}
            <div className="flex flex-col gap-3">
              <p className="m-0">{t(connectMode === "server_dial" ? "add.offlineInstructions" : "add.manualInstructions")}</p>
              {connectMode === "server_dial" && (
                <p className="m-0">
                  <a href={OFFLINE_PACKAGE} download>{t("add.offlineDownload")}</a>
                  {offlineChecksum && <span className="block text-fg-muted">{t("add.offlineChecksum", { sum: offlineChecksum })}</span>}
                </p>
              )}
              {!pendingCommand && <div><Button variant="primary" loading={creatingPending} onClick={() => void createPendingNode()}>{t("add.createCommand")}</Button></div>}
              {pendingCommand && <span className="text-fg-muted">{t("add.tokenSaved")}</span>}
              <pre className="m-0 overflow-x-auto rounded-lg bg-inverse-background p-4 text-fg-on-inverse">
                {cmd}
              </pre>
              <div>
                <Button variant="tertiary" leadingIcon={ConsoleIcon.copy} disabled={!pendingCommand} onClick={() => void copyCommand()}>
                  {t("add.copyCommand")}
                </Button>
              </div>
            </div>
          </>
        )}
      </div>
    </SideDrawer>
  );
}

// Choice is a labeled row of mutually exclusive options.
function Choice({ label, value, onChange, options }: { label: ReactNode; value: string; onChange: (value: string) => void; options: { value: string; label: string }[] }) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="px-1.5 text-body font-medium text-fg-muted">{label}</span>
      <RadioGroup aria-label={typeof label === "string" ? label : undefined} value={value} onValueChange={onChange} className="w-auto flex-row flex-wrap">
        {options.map((option, index) => <RadioItem key={option.value} index={index} value={option.value} label={option.label} />)}
      </RadioGroup>
    </div>
  );
}

function SummaryStrip({
  title,
  summary,
  tone,
}: {
  title: string;
  summary: OnboardingCheckCounts;
  tone: "info" | "warning" | "danger";
}) {
  const { t } = useTranslation("nodes");
  return (
    <Notice tone={tone} title={title}>
      <span className="flex flex-wrap gap-2">
        <Badge status="success">{t("add.check.okCount", { count: summary.pass })}</Badge>
        <Badge status="warning">{t("add.check.warnCount", { count: summary.warn })}</Badge>
        <Badge status="danger">{t("add.check.errorCount", { count: summary.fail })}</Badge>
        {summary.skip > 0 && <Badge status="neutral">{t("add.check.skipCount", { count: summary.skip })}</Badge>}
      </span>
    </Notice>
  );
}

function ReportTable({ title, checks }: { title: string; checks: CheckResult[] }) {
  const { t } = useTranslation("nodes");
  return (
    <section className="flex flex-col gap-2">
      <h3 className="m-0 text-body font-semibold text-fg-default">{title}</h3>
      <OnboardingChecks checks={checks} labels={{ item: t("add.check.item"), status: t("add.check.status"), detail: t("add.check.detail") }} />
    </section>
  );
}
