import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { apiErrorText } from "../apiError";
import type { NetworkRole, NodeManagement, NodeNetworks } from "../networkTypes";
import { Endpoint } from "../pages/networks/networkDisplay";
import { message } from "../ui-kit/feedback";
import { Badge } from "./ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "./ui/card";
import { DetailList, DetailListItem, DetailListLabel, DetailListValue } from "./ui/detail-list";
import { Switch } from "./ui/switch";

const ROLES: NetworkRole[] = ["serving", "storage_sync", "compute"];

/**
 * A node's management network: the address it reaches the server from and
 * whether that network may carry each role where no dedicated network does.
 * Only administrators may switch roles; a failed identity lookup stays
 * read-only.
 */
export default function NodeManagementCard({ nodeUUID, management, onChanged }: {
  nodeUUID: string;
  management: NodeManagement;
  onChanged: (networks: NodeNetworks) => void;
}) {
  const { t } = useTranslation("networks");
  const { t: tCommon } = useTranslation("common");
  const [isAdmin, setIsAdmin] = useState(false);
  const [saving, setSaving] = useState<NetworkRole | null>(null);

  useEffect(() => {
    api.me().then((user) => setIsAdmin(user.role === "admin")).catch(() => {});
  }, []);

  const toggle = async (role: NetworkRole, on: boolean) => {
    const off = on ? management.roles_off.filter((r) => r !== role) : [...management.roles_off, role];
    setSaving(role);
    try {
      onChanged(await api.setManagementRoles(nodeUUID, off));
      message.success(t("management.saved"));
    } catch (e) {
      message.error(apiErrorText(tCommon, e));
    } finally {
      setSaving(null);
    }
  };

  return (
    <Card className="border border-border-subtle">
      <CardHeader><CardTitle>{t("management.title")}</CardTitle></CardHeader>
      <CardContent className="flex flex-col gap-2 pb-4">
        <DetailList>
          <DetailListItem>
            <DetailListLabel>{t("management.address")}</DetailListLabel>
            <DetailListValue>
              {management.ip ? (
                <span className="inline-flex flex-wrap items-center justify-end gap-1">
                  <Endpoint iface={management.interface} ip={management.ip} />
                  {management.behind_nat && <Badge size="sm" color="orange">{t("management.nat")}</Badge>}
                </span>
              ) : <span className="text-fg-muted">{t("management.notReported")}</span>}
            </DetailListValue>
          </DetailListItem>
          {management.remote_addr && management.behind_nat && (
            <DetailListItem>
              <DetailListLabel>{t("management.remote")}</DetailListLabel>
              <DetailListValue><code className="rounded-sm bg-emphasis px-1 font-mono text-label">{management.remote_addr}</code></DetailListValue>
            </DetailListItem>
          )}
          <DetailListItem className="items-start">
            <DetailListLabel>{t("management.canCarry")}</DetailListLabel>
            <DetailListValue className="flex flex-col items-end">
              {ROLES.map((role) => (
                <Switch
                  key={role}
                  className="px-0 py-1"
                  label={t(`management.roleSwitch.${role}`)}
                  checked={!management.roles_off.includes(role)}
                  disabled={!isAdmin || saving !== null}
                  aria-busy={saving === role || undefined}
                  onCheckedChange={(on) => void toggle(role, on)}
                />
              ))}
            </DetailListValue>
          </DetailListItem>
        </DetailList>
        <span className="text-fg-muted">{t("management.roleHint")}</span>
      </CardContent>
    </Card>
  );
}
