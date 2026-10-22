import { useEffect, useState } from "react";
import { Alert, Button, Form, Input, InputNumber, Modal, Space, Spin, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, usePolling } from "../../api";
import { storageErrorText } from "../../storageDisplay";

const GIB = 1024 ** 3;

function SettingsForm({ isAdmin }: { isAdmin: boolean }) {
  const { t } = useTranslation("storage");
  const settings = usePolling(api.storageSettings, 0);
  const [name, setName] = useState("");
  const [reserveGiB, setReserveGiB] = useState<number | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const loaded = settings.data;
  useEffect(() => {
    if (!loaded) return;
    setName(loaded.control_plane_name);
    // Zero means "the default": the smaller of 10% of each disk and 50 GiB.
    setReserveGiB(loaded.reserve_free_bytes ? Math.round((loaded.reserve_free_bytes / GIB) * 10) / 10 : null);
  }, [loaded]);
  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      await api.updateStorageSettings({ control_plane_name: name.trim(), reserve_free_bytes: Math.round((reserveGiB ?? 0) * GIB) });
      message.success(t("settings.saved"));
      void settings.refetch();
    } catch (e) {
      setError(storageErrorText(t, e));
    } finally {
      setSaving(false);
    }
  };
  if (settings.error) {
    return <Alert type="error" showIcon message={t("settings.loadFailed")} description={settings.error.message} action={<Button size="small" onClick={() => void settings.refetch()}>{t("settings.retry")}</Button>} />;
  }
  if (!loaded) return <Spin aria-label={t("settings.loading")} />;
  return (
    <>
      {error && <Alert type="error" showIcon message={t("settings.saveFailed")} description={error} style={{ marginBottom: 12 }} />}
      <Form layout="vertical" onFinish={() => void save()}>
        <Form.Item label={t("settings.controlPlaneName")} htmlFor="storage-cp-name" help={t("settings.controlPlaneNameHelp")}>
          <Input id="storage-cp-name" value={name} disabled={!isAdmin} onChange={(e) => setName(e.target.value)} />
        </Form.Item>
        <Form.Item label={t("settings.reserve")} htmlFor="storage-reserve" help={t("settings.reserveHelp")}>
          <Space><InputNumber id="storage-reserve" min={0} value={reserveGiB} disabled={!isAdmin} onChange={setReserveGiB} placeholder={t("settings.reserveDefault")} style={{ width: 260 }} /><span>GiB</span></Space>
        </Form.Item>
        <Typography.Text type="secondary" style={{ display: "block", marginBottom: 8 }}>{t("settings.controlPlaneId", { id: loaded.control_plane_id })}</Typography.Text>
        {isAdmin && <Button type="primary" htmlType="submit" loading={saving}>{t("settings.save")}</Button>}
      </Form>
    </>
  );
}

/**
 * Platform-wide storage settings: the name other gpuworks see for this
 * installation, and the free space kept on node disks. Admins edit; others read.
 */
export default function StorageSettingsModal({ open, isAdmin, onClose }: { open: boolean; isAdmin: boolean; onClose: () => void }) {
  const { t } = useTranslation("storage");
  return (
    <Modal title={t("settings.title")} open={open} onCancel={onClose} footer={null} destroyOnHidden>
      <SettingsForm isAdmin={isAdmin} />
    </Modal>
  );
}
