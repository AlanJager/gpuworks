import { useState } from "react";
import { Alert, Form, Input, Modal } from "antd";
import { useTranslation } from "react-i18next";
import { storageErrorText } from "../../storageDisplay";
import { validEntryName } from "../../storageTypes";

/**
 * Asks for one entry name (a new directory, or an entry's new name) and runs
 * the change; the server's refusal stays in the dialog so the name can be
 * corrected.
 */
export default function StorageNameDialog({ title, okText, initial = "", onSubmit, onClose }: {
  title: string;
  okText: string;
  initial?: string;
  onSubmit: (name: string) => Promise<void>;
  onClose: () => void;
}) {
  const { t } = useTranslation("storage");
  const [name, setName] = useState(initial);
  const [touched, setTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const trimmed = name.trim();
  const valid = validEntryName(trimmed);
  const submit = async () => {
    setTouched(true);
    if (!valid || busy) return;
    setBusy(true);
    setError(null);
    try {
      await onSubmit(trimmed);
    } catch (e) {
      setError(storageErrorText(t, e));
      setBusy(false);
    }
  };
  return (
    <Modal
      open
      title={title}
      okText={okText}
      cancelText={t("browse.cancel")}
      okButtonProps={{ disabled: !valid || trimmed === initial, loading: busy }}
      onOk={() => void submit()}
      onCancel={onClose}
    >
      <Form layout="vertical" onFinish={() => void submit()}>
        <Form.Item
          label={t("browse.name")}
          htmlFor="storage-entry-name"
          validateStatus={touched && !valid ? "error" : undefined}
          help={touched && !valid ? t("browse.nameInvalid") : undefined}
        >
          <Input id="storage-entry-name" autoFocus value={name} onChange={(e) => { setName(e.target.value); setTouched(true); }} />
        </Form.Item>
      </Form>
      {error && <Alert type="error" showIcon message={error} />}
    </Modal>
  );
}
