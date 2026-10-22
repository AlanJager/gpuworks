import { useEffect, useState } from "react";
import { Card, Descriptions, Space, Switch, Tag, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { apiErrorText } from "../apiError";
import type { NetworkRole, NodeManagement, NodeNetworks } from "../networkTypes";
import { Endpoint } from "../pages/networks/networkDisplay";

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
    <Card size="small" title={t("management.title")}>
      <Descriptions column={1} size="small">
        <Descriptions.Item label={t("management.address")}>
          {management.ip ? (
            <Space size={4}>
              <Endpoint iface={management.interface} ip={management.ip} />
              {management.behind_nat && <Tag color="orange">{t("management.nat")}</Tag>}
            </Space>
          ) : <Typography.Text type="secondary">{t("management.notReported")}</Typography.Text>}
        </Descriptions.Item>
        {management.remote_addr && management.behind_nat && (
          <Descriptions.Item label={t("management.remote")}><Typography.Text code>{management.remote_addr}</Typography.Text></Descriptions.Item>
        )}
        <Descriptions.Item label={t("management.canCarry")}>
          <Space direction="vertical" size={4}>
            {ROLES.map((role) => (
              <Space key={role} size={8}>
                <Switch
                  size="small"
                  aria-label={t(`management.roleSwitch.${role}`)}
                  checked={!management.roles_off.includes(role)}
                  disabled={!isAdmin || (saving !== null && saving !== role)}
                  loading={saving === role}
                  onChange={(on) => void toggle(role, on)}
                />
                <span>{t(`management.roleSwitch.${role}`)}</span>
              </Space>
            ))}
          </Space>
        </Descriptions.Item>
      </Descriptions>
      <Typography.Text type="secondary">{t("management.roleHint")}</Typography.Text>
    </Card>
  );
}
