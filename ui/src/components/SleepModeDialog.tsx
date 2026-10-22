import { Modal, Radio, Space, Typography } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";

/** What the engine does with the requests it is still serving when told to sleep. */
export type SleepMode = "wait" | "abort";

export const defaultSleepMode: SleepMode = "wait";

/**
 * Asks how in-flight requests should be treated before a sleep order is sent.
 * `wait` lets them finish and only queues new ones; `abort` cuts them off.
 * vLLM's third mode (`keep`, freeze and resume) is built for RL loops that sleep
 * for seconds and is deliberately not offered.
 */
export default function SleepModeDialog({ open, level, onConfirm, onCancel, confirming = false }: {
  open: boolean;
  level: 1 | 2;
  onConfirm: (mode: SleepMode) => void;
  onCancel: () => void;
  confirming?: boolean;
}) {
  const { t } = useTranslation("deployments");
  const [mode, setMode] = useState<SleepMode>(defaultSleepMode);
  return (
    <Modal
      open={open}
      title={level === 2 ? t("sleepMode.titleDeep") : t("sleepMode.titleQuick")}
      okText={t("sleepMode.confirm")}
      cancelText={t("sleepMode.cancel")}
      confirmLoading={confirming}
      onOk={() => onConfirm(mode)}
      onCancel={onCancel}
      destroyOnHidden
    >
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Typography.Text type="secondary">{t("sleepMode.intro")}</Typography.Text>
        <Radio.Group value={mode} onChange={(e) => setMode(e.target.value as SleepMode)}>
          <Space direction="vertical">
            <Radio value="wait">
              <Typography.Text strong>{t("sleepMode.wait")}</Typography.Text>
              <br />
              <Typography.Text type="secondary">{t("sleepMode.waitDetail")}</Typography.Text>
            </Radio>
            <Radio value="abort">
              <Typography.Text strong>{t("sleepMode.abort")}</Typography.Text>
              <br />
              <Typography.Text type="secondary">{t("sleepMode.abortDetail")}</Typography.Text>
            </Radio>
          </Space>
        </Radio.Group>
      </Space>
    </Modal>
  );
}
