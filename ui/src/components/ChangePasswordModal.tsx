import { Alert, Form, Input, Modal, Typography } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type AuthUser } from "../api";
import { APIError, apiErrorText } from "../apiError";

type Values = { current: string; next: string; confirm: string };

/** Errors that belong to one field; anything else is shown above the form. */
const FIELD_ERRORS: Record<string, keyof Values> = {
  CURRENT_PASSWORD_INVALID: "current",
  PASSWORD_LENGTH: "next",
  PASSWORD_CHARACTERS: "next",
  PASSWORD_GUESSABLE: "next",
  PASSWORD_USERNAME: "next",
  PASSWORD_UNCHANGED: "next",
};

/**
 * ChangePasswordModal replaces the signed-in user's password. The server ends
 * every session of the user on success, so onChanged signs this tab out.
 */
export default function ChangePasswordModal({ open, policy, onClose, onChanged }: {
  open: boolean;
  policy?: AuthUser["password_policy"];
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t } = useTranslation("auth");
  const { t: tCommon } = useTranslation("common");
  const [form] = Form.useForm<Values>();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  async function submit(values: Values) {
    setSaving(true);
    setError("");
    try {
      await api.changePassword(values.current, values.next);
      onChanged();
    } catch (err) {
      const text = apiErrorText(tCommon, err);
      const field = err instanceof APIError ? FIELD_ERRORS[err.code] : undefined;
      if (field) form.setFields([{ name: field, errors: [text] }]);
      else setError(text);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      open={open}
      title={t("changePassword.title")}
      okText={t("changePassword.submit")}
      onOk={() => form.submit()}
      onCancel={onClose}
      confirmLoading={saving}
      afterClose={() => { form.resetFields(); setError(""); }}
      destroyOnHidden
    >
      <Typography.Paragraph type="secondary">{t("changePassword.description")}</Typography.Paragraph>
      {error && <Alert type="error" showIcon message={error} role="alert" style={{ marginBottom: 16 }} />}
      <Form form={form} layout="vertical" onFinish={submit} requiredMark={false}>
        <Form.Item label={t("changePassword.current")} name="current" rules={[{ required: true, message: t("changePassword.currentRequired") }]}>
          <Input.Password autoComplete="current-password" />
        </Form.Item>
        <Form.Item
          label={t("changePassword.next")}
          name="next"
          extra={policy ? t("changePassword.rule", { min: policy.min_length, max: policy.max_length }) : undefined}
          rules={[{ required: true, message: t("changePassword.nextRequired") }]}
        >
          <Input.Password autoComplete="new-password" />
        </Form.Item>
        <Form.Item
          label={t("changePassword.confirm")}
          name="confirm"
          dependencies={["next"]}
          rules={[
            { required: true, message: t("changePassword.confirmRequired") },
            ({ getFieldValue }) => ({
              validator: (_, value) => !value || value === getFieldValue("next")
                ? Promise.resolve()
                : Promise.reject(new Error(t("changePassword.mismatch"))),
            }),
          ]}
        >
          <Input.Password autoComplete="new-password" />
        </Form.Item>
      </Form>
    </Modal>
  );
}
