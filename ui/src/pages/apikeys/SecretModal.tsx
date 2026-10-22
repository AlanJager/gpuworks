import { Alert, Modal, Typography, theme } from "antd";
import { useTranslation } from "react-i18next";

const { Paragraph } = Typography;

/** SecretModal shows a just-created key or token, the only time it is shown. */
export default function SecretModal({ secret, onClose }: { secret: string | null; onClose: () => void }) {
  const { t } = useTranslation("apikeys");
  const { token } = theme.useToken();
  return (
    <Modal
      title={t("secret.title")}
      open={!!secret}
      destroyOnHidden
      onCancel={onClose}
      onOk={onClose}
      okText={t("secret.saved")}
      cancelText={t("actions.close")}
    >
      <Alert
        type="warning"
        showIcon
        message={t("secret.warning")}
        style={{ marginBottom: token.marginSM }}
      />
      <Paragraph
        copyable={{ text: secret ?? "", tooltips: [t("secret.copy"), t("secret.copied")] }}
        style={{
          background: token.colorFillTertiary,
          padding: token.paddingSM,
          borderRadius: token.borderRadius,
          wordBreak: "break-all",
          fontFamily: token.fontFamilyCode,
        }}
      >
        {secret}
      </Paragraph>
    </Modal>
  );
}
