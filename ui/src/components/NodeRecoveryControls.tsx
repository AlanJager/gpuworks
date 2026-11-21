import { useEffect, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { api, useEventPayload, type Node, type SSHCredentialInfo } from "../api";
import SSHCredentialFields, { type SSHAuthMode } from "./SSHCredentialFields";
import { onboardingCheckText } from "./OnboardingChecks";
import { recoveryLabel } from "../nodes/recoveryPhase";
import { recoveryErrorText } from "../nodes/recoveryError";
import { formatTime, formatTimeSec } from "../i18n/datetime";
import { ConfirmPopover } from "../ui-kit/ConfirmPopover";
import { message } from "../ui-kit/feedback";
import { FormField } from "../ui-kit/form/FormField";
import { formSubmit } from "../ui-kit/form/submit";
import { NumberInput } from "../ui-kit/NumberInput";
import { Timeline } from "../ui-kit/Timeline";
import { Badge, type BadgeProps } from "./ui/badge";
import { Button } from "./ui/button";
import { Checkbox } from "./ui/checkbox";
import { DetailList, DetailListItem, DetailListLabel, DetailListValue } from "./ui/detail-list";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";
import { Input } from "./ui/input";

type Updated = (node: Node) => void;
type SSHValues = { host: string; port: number | null; user: string; password?: string; private_key_pem?: string; probe: boolean };
const BLANK: SSHValues = { host: "", port: 22, user: "root", password: undefined, private_key_pem: undefined, probe: true };
const DIALOG_STYLE = { width: "min(520px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" };

const authModeLabel: Record<SSHAuthMode, string> = {
  platform_key: "ssh.auth.platformKey",
  password: "ssh.auth.password",
  keyboard_interactive: "ssh.auth.keyboardInteractive",
  private_key: "ssh.auth.key",
};

// How a recovery phase's tag colour reads as a badge: a state where the
// phase has one, otherwise the same categorical colour.
function phaseBadge(color?: string): Pick<BadgeProps, "status" | "color"> {
  switch (color) {
    case "processing": return { status: "info" };
    case "error": return { status: "danger" };
    case "blue": return { color: "blue" };
    case "purple": return { color: "purple" };
    default: return { status: "neutral" };
  }
}

const required = (text: string) => ({ validate: (value: unknown) => (typeof value === "string" && value.trim() !== "") || text });

export function NodeSSHAccess({ node, onUpdated }: { node: Node; onUpdated: Updated }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const form = useForm<SSHValues>({ shouldFocusError: false, defaultValues: BLANK });
  const formRef = useRef<HTMLFormElement>(null);
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [authMode, setAuthMode] = useState<SSHAuthMode>("platform_key");
  const [info, setInfo] = useState<SSHCredentialInfo | null>(null);

  const openEditor = async () => {
    let existing: SSHCredentialInfo | null = null;
    try {
      existing = await api.nodeSSH(node.uuid);
    } catch {
      // A 404 means this pre-existing node has no stored side channel yet.
    }
    setInfo(existing);
    setAuthMode(existing?.auth_mode ?? "platform_key");
    form.reset({
      host: existing?.host || node.hostname || "",
      port: existing?.port || 22,
      user: existing?.user || "root",
      password: undefined,
      private_key_pem: undefined,
      probe: true,
    });
    setOpen(true);
  };

  const save = async (values: SSHValues) => {
    setSaving(true);
    try {
      const saved = await api.putNodeSSH(node.uuid, {
        host: values.host.trim(),
        port: values.port ?? 22,
        user: values.user.trim(),
        probe: values.probe,
        auth_mode: authMode,
        password: authMode === "password" || authMode === "keyboard_interactive" ? values.password : undefined,
        private_key_pem: authMode === "private_key" ? values.private_key_pem?.trim() || undefined : undefined,
      });
      setInfo(saved);
      onUpdated(await api.node(node.uuid));
      setOpen(false);
      if (saved.platform_key?.status === "pass") {
        message.success(t("ssh.message.switchedToPlatformKey"));
      } else if (saved.platform_key) {
        message.warning(t("ssh.message.platformKeyNotAuthorized", { detail: onboardingCheckText(tCommon, saved.platform_key) }));
      }
      if (saved.probe?.foreign_server_url) {
        message.warning(t("ssh.message.savedProbeForeignAgent", { server: saved.probe.foreign_server_url }));
      } else if (saved.probe && !saved.probe.reachable) {
        message.warning(t("ssh.message.savedProbeFailed", { detail: saved.probe.detail || t("recovery.phase.unreachable") }));
      } else {
        message.success(t("ssh.message.saved"));
      }
    } catch (error) {
      message.error(t("ssh.message.saveFailed", { detail: (error as Error).message }));
    } finally {
      setSaving(false);
    }
  };

  const remove = async () => {
    setDeleting(true);
    try {
      await api.deleteNodeSSH(node.uuid);
      setInfo(null);
      onUpdated(await api.node(node.uuid));
      setOpen(false);
      message.success(t("ssh.message.cleared"));
    } catch (error) {
      message.error(t("ssh.message.clearFailed", { detail: (error as Error).message }));
    } finally {
      setDeleting(false);
    }
  };

  return (
    <>
      <span className="inline-flex items-center gap-2">
        {node.has_ssh_credential ? <Badge status="success">{t("ssh.configured")}</Badge> : <span className="text-fg-muted">{t("ssh.notConfigured")}</span>}
        <Button variant="tertiary" size="sm" onClick={() => void openEditor()}>{t(node.has_ssh_credential ? "ssh.edit" : "ssh.configure")}</Button>
      </span>
      {/* The dialog stays open while a save is in flight: its result (and the
          probe's) is reported against what the operator just entered. */}
      <Dialog open={open} onOpenChange={(next) => { if (!next && !saving) setOpen(false); }}>
        <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={saving} style={DIALOG_STYLE}>
          <DialogHeader><DialogTitle>{t("ssh.title")}</DialogTitle></DialogHeader>
          <form ref={formRef} noValidate onSubmit={formSubmit(form, () => formRef.current, save)} className="flex flex-col gap-4">
            <FormField control={form.control} name="host" required label={t("ssh.host")} rules={required(t("ssh.hostRequired"))}>
              {(binding) => <Input {...binding} placeholder="gpu.example.com" />}
            </FormField>
            <div className="flex items-start gap-3">
              <div className="w-32 shrink-0">
                <FormField control={form.control} name="port" required label={t("ssh.port")} rules={{ validate: (value) => value != null || t("ssh.portRequired") }}>
                  {(binding) => <NumberInput {...binding} min={1} max={65535} precision={0} />}
                </FormField>
              </div>
              <div className="min-w-0 flex-1">
                <FormField control={form.control} name="user" required label={t("ssh.user")} rules={required(t("ssh.userRequired"))}>
                  {(binding) => <Input {...binding} placeholder="root" />}
                </FormField>
              </div>
            </div>
            <SSHCredentialFields control={form.control} authMode={authMode} onAuthModeChange={setAuthMode} />
            <label className="inline-flex items-center gap-2">
              <Checkbox checked={form.watch("probe")} onCheckedChange={(checked) => form.setValue("probe", checked === true)} />
              <span>{t("ssh.probeAfterSave")}</span>
            </label>
            {info && <p className="text-fg-muted">{t("ssh.existingAuth", { method: t(authModeLabel[info.auth_mode]) })}</p>}
            {info?.auth_mode === "password" && <p className="text-fg-muted">{t("ssh.savedPasswordNote")}</p>}
            <DialogFooter className="justify-between">
              <span>{info && <ConfirmPopover title={t("ssh.clearConfirm")} destructive onConfirm={remove}
                trigger={<Button type="button" variant="destructive" loading={deleting}>{t("ssh.clear")}</Button>} />}</span>
              <span className="flex gap-2">
                <Button type="button" variant="ghost" disabled={saving} onClick={() => setOpen(false)}>{t("ssh.cancel")}</Button>
                <Button type="submit" variant="primary" loading={saving}>{t("ssh.save")}</Button>
              </span>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </>
  );
}

export function NodeRecoveryStatus({ node, onUpdated }: { node: Node; onUpdated: Updated }) {
  const { t } = useTranslation("nodes");
  const [events, setEvents] = useState<Array<{ at: Date; reason: string }>>([]);

  useEffect(() => setEvents([]), [node.uuid]);
  useEventPayload("node", (event) => {
    if (event.key !== node.uuid || !event.reason?.startsWith("recovery:")) return;
    setEvents((current) => [...current.slice(-9), { at: new Date(), reason: event.reason ?? "recovery" }]);
    api.node(node.uuid).then(onUpdated).catch(() => {});
  });

  if (!node.recovery_phase) return null;

  const label = recoveryLabel[node.recovery_phase];
  return (
    <section className="mb-4" aria-label={t("recovery.title")}>
      <h5 className="mb-2 text-body font-semibold text-fg-default">{t("recovery.title")}</h5>
      <DetailList>
        <DetailListItem><DetailListLabel>{t("recovery.currentPhase")}</DetailListLabel><DetailListValue><Badge {...phaseBadge(label?.color)}>{label ? t(label.key) : node.recovery_phase}</Badge></DetailListValue></DetailListItem>
        <DetailListItem><DetailListLabel>{t("recovery.attempts")}</DetailListLabel><DetailListValue>{node.recovery_attempts ?? 0}</DetailListValue></DetailListItem>
        <DetailListItem><DetailListLabel>{t("recovery.lastProbe")}</DetailListLabel><DetailListValue>{formatTime(node.last_probe_at)}</DetailListValue></DetailListItem>
        {node.last_recovery_error && <DetailListItem><DetailListLabel>{t("recovery.lastError")}</DetailListLabel><DetailListValue className="whitespace-pre-line text-fg-danger">{recoveryErrorText(t, node.last_recovery_code, node.last_recovery_error)}</DetailListValue></DetailListItem>}
      </DetailList>
      {events.length > 0 && (
        <div className="mt-4">
          <Timeline
            label={t("recovery.title")}
            items={events.map((event, index) => { const eventLabel = recoveryLabel[event.reason.slice("recovery:".length)]; return { key: `${index}-${event.at.getTime()}`, title: `${formatTimeSec(event.at)} · ${eventLabel ? t(eventLabel.key) : event.reason}` }; })}
          />
        </div>
      )}
    </section>
  );
}
