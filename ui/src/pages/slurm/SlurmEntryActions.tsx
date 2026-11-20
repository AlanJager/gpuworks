import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import type { SlurmCluster } from "../../slurmApi";
import { useCopy } from "../../components/CopyButton";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { confirm, message } from "../../ui-kit/feedback";
import RowActions, { type RowAction } from "../platforms/RowActions";
import SlurmApplyModal from "./SlurmApplyModal";
import SlurmClusterDrawer from "./SlurmClusterDrawer";
import SlurmDeleteDialog from "./SlurmDeleteDialog";
import SlurmScriptModal from "./SlurmScriptModal";
import SlurmUninstallModal from "./SlurmUninstallModal";

/**
 * What an administrator does to a Slurm entry: editing it; for an entry made
 * from a cloud account's cluster, checking and applying (shown disabled, with
 * the server's reason, when the platform cannot); copying the setup command;
 * reading the setup script; uninstalling the agents and the health check;
 * revoking the enrollment token; deleting the entry. The common ones stay in
 * the row and the rest fold into a menu, the destructive ones last. Whether
 * the platform can apply and why not, and whether the entry can be deleted
 * with its nodes, are the server's word on the entry. The buttons carry their
 * names, and applying is the filled one while no node has joined.
 */
// labeled writes the buttons' names beside their icons: right for a cluster
// group's header, which has the room; a table row's 操作 cell does not, so a
// row keeps icon buttons with their names as tooltips, like a Kubernetes row.
export default function SlurmEntryActions({ cluster, onChanged, labeled = false }: { cluster: SlurmCluster; onChanged: () => void; labeled?: boolean }) {
  const { t } = useTranslation("slurm");
  const { t: tCommon } = useTranslation("common");
  const [copy, copyHolder] = useCopy();
  const [editing, setEditing] = useState(false);
  const [applying, setApplying] = useState(false);
  const [script, setScript] = useState(false);
  const [uninstalling, setUninstalling] = useState(false);
  const [deleting, setDeleting] = useState(false);

  const confirmRevoke = () => confirm({
    title: t("actions.revokeConfirm", { name: cluster.name }),
    okText: t("actions.revoke"),
    cancelText: t("actions.cancel"),
    okButtonProps: { danger: true },
    content: <p className="m-0 text-fg-muted">{t("actions.revokeHelp")}</p>,
    onOk: async () => {
      try {
        await api.revokeSlurmCluster(cluster.uuid);
        message.success(t("actions.revoked", { name: cluster.name }));
        onChanged();
      } catch (e) {
        // The dialog stays open on the reason, so it can be retried or cancelled.
        throw new Error(`${t("actions.failed")}: ${apiErrorText(tCommon, e)}`);
      }
    },
  });

  const actions: RowAction[] = [{ key: "edit", label: t("actions.edit"), icon: ConsoleIcon.edit, common: true, onSelect: () => setEditing(true) }];
  if (cluster.source) {
    const blocked = cluster.operation_blocked ? t(`blocked.${cluster.operation_blocked}`) : "";
    // No node has joined yet (the server's state): applying is the next step, so it leads.
    const next = cluster.state === "pending" || cluster.state === "stalled";
    actions.push({ key: "apply", label: t("actions.apply"), icon: ConsoleIcon.upload, common: true, primary: next, disabled: !cluster.can_apply, hint: blocked, onSelect: () => setApplying(true) });
  }
  if (cluster.setup_command) {
    const command = cluster.setup_command;
    actions.push({ key: "copy", label: t("actions.copyCommand"), icon: ConsoleIcon.copy, common: !cluster.source, disabled: cluster.revoked,
      onSelect: () => void copy(command, { success: t("actions.commandCopied") }) });
  }
  actions.push({ key: "script", label: t("actions.script"), icon: ConsoleIcon.manifest, onSelect: () => setScript(true) });
  actions.push(
    { key: "uninstall", label: t("actions.uninstall"), icon: ConsoleIcon.terminal, danger: true, onSelect: () => setUninstalling(true) },
    { key: "revoke", label: t("actions.revoke"), icon: ConsoleIcon.block, danger: true, disabled: cluster.revoked, onSelect: confirmRevoke },
    { key: "delete", label: t("actions.delete"), icon: ConsoleIcon.delete, danger: true, onSelect: () => setDeleting(true) },
  );

  return (
    <>
      <RowActions actions={actions} label={tCommon("resourceTable.actions")} labeled={labeled} />
      {copyHolder}
      {editing && <SlurmClusterDrawer key={cluster.uuid} open edit={cluster} onClose={() => setEditing(false)} onCreated={onChanged} onEdited={onChanged} />}
      {applying && <SlurmApplyModal cluster={cluster} onClose={() => setApplying(false)} onDone={onChanged} />}
      {script && <SlurmScriptModal cluster={cluster} onClose={() => setScript(false)} />}
      {uninstalling && (
        <SlurmUninstallModal cluster={cluster} onClose={() => setUninstalling(false)} onChanged={onChanged}
          onDelete={() => { setUninstalling(false); setDeleting(true); }} />
      )}
      {deleting && (
        <SlurmDeleteDialog cluster={cluster} onClose={() => setDeleting(false)} onDeleted={onChanged}
          onUninstall={() => { setDeleting(false); setUninstalling(true); }} />
      )}
    </>
  );
}
