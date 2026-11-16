import { Alert, Skeleton, Space, Switch, Typography } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type Node, type NodePlacementPolicySetting, type NodePlacementPolicyValue } from "../api";

/**
 * NodePlacementPolicy edits a node's placement policy: the per-node knobs on
 * how the scheduler may place work there. Which knobs exist, their kind and
 * default come from the server's dictionary (GET
 * /scheduling/node-placement-policies), and whether each takes effect on this
 * node from the node itself, so a knob the server adds needs only its
 * label and help (nodes:placementPolicy.<key>.*). A knob that has no effect
 * on the node now can still be set; the server says why it does not apply.
 */
export default function NodePlacementPolicy({ node, onUpdated }: { node: Node; onUpdated: (node: Node) => void }) {
  const { t } = useTranslation("nodes");
  const [settings, setSettings] = useState<NodePlacementPolicySetting[] | null>(null);
  const [loadError, setLoadError] = useState<Error | null>(null);
  const [values, setValues] = useState<Record<string, NodePlacementPolicyValue>>(node.placement_policy ?? {});
  const [saving, setSaving] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api.nodePlacementPolicyDictionary()
      .then((dictionary) => { if (!cancelled) setSettings(dictionary); })
      .catch((error) => { if (!cancelled) setLoadError(error as Error); });
    return () => { cancelled = true; };
  }, []);
  useEffect(() => { setValues(node.placement_policy ?? {}); }, [node.placement_policy]);

  if (loadError) return <Alert type="error" showIcon message={t("placementPolicy.loadFailed")} description={loadError.message} />;
  if (!settings) return <Skeleton active paragraph={{ rows: 1 }} title={false} />;
  if (settings.length === 0) return <Typography.Text type="secondary">{t("placementPolicy.empty")}</Typography.Text>;

  const save = async (key: string, value: NodePlacementPolicyValue) => {
    const previous = values;
    setValues({ ...values, [key]: value });
    setSaving(key);
    setSaveError(null);
    try {
      onUpdated(await api.setNodePlacementPolicy(node.uuid, { [key]: value }));
    } catch (error) {
      setValues(previous);
      setSaveError(t("placementPolicy.saveFailed", { detail: (error as Error).message }));
    } finally {
      setSaving(null);
    }
  };

  return (
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      {saveError && <Alert type="error" showIcon message={saveError} />}
      {settings.map((setting) => {
        const value = values[setting.key] ?? setting.default;
        const availability = node.placement_policy_availability?.find((item) => item.key === setting.key);
        const label = t(`placementPolicy.${setting.key}.label`, { defaultValue: setting.key });
        const labelID = `placement-policy-${setting.key}`;
        return (
          <Space key={setting.key} direction="vertical" size={4} style={{ width: "100%" }}>
            <Space>
              {setting.kind === "bool" ? (
                <Switch
                  aria-labelledby={labelID}
                  checked={value === true}
                  loading={saving === setting.key}
                  onChange={(checked) => void save(setting.key, checked)}
                />
              ) : (
                <Typography.Text code>{String(value)}</Typography.Text>
              )}
              <Typography.Text id={labelID}>{label}</Typography.Text>
            </Space>
            <Typography.Text type="secondary">{t(`placementPolicy.${setting.key}.help`, { defaultValue: "" })}</Typography.Text>
            {availability && !availability.applicable && (
              <Alert
                type="warning"
                showIcon
                message={t("placementPolicy.notApplicable", {
                  reason: t(`placementPolicy.reason.${availability.reason}`, { defaultValue: availability.reason ?? "" }),
                })}
              />
            )}
          </Space>
        );
      })}
    </Space>
  );
}
