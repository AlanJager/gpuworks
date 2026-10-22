import { useEffect, useState } from "react";
import { Alert, Button, Card, Descriptions, Input, Modal, Popconfirm, Space, Spin, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type PlatformSSHKey } from "../../api";
import { apiErrorText } from "../../apiError";
import { formatTime } from "../../i18n/datetime";

// PlatformSSHKeyCard shows the server's own SSH key: the public half nodes
// authorize, and the two ways to replace it.
export default function PlatformSSHKeyCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [key, setKey] = useState<PlatformSSHKey | null>(null);
  const [loadError, setLoadError] = useState("");
  const [rotating, setRotating] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [importPEM, setImportPEM] = useState("");
  const [importing, setImporting] = useState(false);

  useEffect(() => {
    api.platformSSHKey()
      .then((value) => {
        setKey(value);
        setLoadError("");
      })
      .catch((error) => setLoadError(apiErrorText(tCommon, error)));
  }, [tCommon]);

  const rotate = async () => {
    setRotating(true);
    try {
      setKey(await api.rotatePlatformSSHKey());
      message.success(t("sshKey.rotated"));
    } catch (error) {
      message.error(t("sshKey.rotateFailed", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setRotating(false);
    }
  };

  const importKey = async () => {
    setImporting(true);
    try {
      setKey(await api.importPlatformSSHKey(importPEM));
      setImportOpen(false);
      setImportPEM("");
      message.success(t("sshKey.imported"));
    } catch (error) {
      message.error(t("sshKey.importFailed", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setImporting(false);
    }
  };

  return (
    <Card
      title={t("sshKey.title")}
      extra={
        <Space>
          <Button onClick={() => setImportOpen(true)}>{t("sshKey.import")}</Button>
          <Popconfirm title={t("sshKey.rotateConfirm")} description={t("sshKey.rotateWarning")} onConfirm={() => void rotate()}>
            <Button danger loading={rotating}>{t("sshKey.rotate")}</Button>
          </Popconfirm>
        </Space>
      }
    >
      <Typography.Paragraph type="secondary">{t("sshKey.description")}</Typography.Paragraph>
      {loadError && <Alert type="error" showIcon message={t("sshKey.loadFailed", { detail: loadError })} />}
      {!loadError && !key && <Spin size="small" />}
      {key && (
        <Descriptions column={1} size="small" bordered>
          <Descriptions.Item label={t("sshKey.publicKey")}>
            <Typography.Paragraph copyable code style={{ marginBottom: 0, wordBreak: "break-all" }}>{key.public_key}</Typography.Paragraph>
          </Descriptions.Item>
          <Descriptions.Item label={t("sshKey.fingerprint")}>{key.fingerprint}</Descriptions.Item>
          <Descriptions.Item label={t("sshKey.createdAt")}>{formatTime(key.created_at)}</Descriptions.Item>
          <Descriptions.Item label={t("sshKey.authorizeCommand")}>
            <Typography.Paragraph copyable code style={{ marginBottom: 0, wordBreak: "break-all" }}>{key.authorize_command}</Typography.Paragraph>
          </Descriptions.Item>
        </Descriptions>
      )}
      <Modal
        title={t("sshKey.importTitle")}
        open={importOpen}
        onCancel={() => setImportOpen(false)}
        onOk={() => void importKey()}
        okText={t("sshKey.import")}
        okButtonProps={{ disabled: !importPEM.trim() }}
        confirmLoading={importing}
        destroyOnClose
      >
        <Typography.Paragraph type="secondary">{t("sshKey.importHelp")}</Typography.Paragraph>
        <Input.TextArea
          rows={8}
          value={importPEM}
          onChange={(event) => setImportPEM(event.target.value)}
          placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
          aria-label={t("sshKey.privateKey")}
        />
      </Modal>
    </Card>
  );
}
