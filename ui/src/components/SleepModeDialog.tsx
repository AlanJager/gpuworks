import { Checkbox, Modal, Radio, Space, Typography } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

/** What the engine does with the requests it is still serving when told to sleep. */
export type SleepMode = "wait" | "abort";

export const defaultSleepMode: SleepMode = "wait";

/**
 * Asks how in-flight requests should be treated before a sleep order is sent.
 * `wait` lets them finish and only queues new ones; `abort` cuts them off.
 * vLLM's third mode (`keep`, freeze and resume) is built for RL loops that sleep
 * for seconds and is deliberately not offered.
 *
 * Given wakeOnRequest (the server's default for the deployment), it also asks
 * whether a request wakes the sleeper again; without it, the order leaves
 * that to the server's default for each deployment.
 */
export default function SleepModeDialog({ open, level, onConfirm, onCancel, confirming = false, wakeOnRequest }: {
  open: boolean;
  level: 1 | 2;
  onConfirm: (mode: SleepMode, wakeOnRequest?: boolean) => void;
  onCancel: () => void;
  confirming?: boolean;
  wakeOnRequest?: boolean;
}) {
  const { t } = useTranslation("deployments");
  const [mode, setMode] = useState<SleepMode>(defaultSleepMode);
  const [wakes, setWakes] = useState<boolean | undefined>(undefined);
  // The server's default until the operator chooses, each time the dialog opens.
  useEffect(() => { if (open) setWakes(undefined); }, [open]);
  const chosenWakes = wakes ?? wakeOnRequest;
  return (
    <Modal
      open={open}
      title={level === 2 ? t("sleepMode.titleDeep") : t("sleepMode.titleQuick")}
      okText={t("sleepMode.confirm")}
      cancelText={t("sleepMode.cancel")}
      confirmLoading={confirming}
      onOk={() => onConfirm(mode, chosenWakes)}
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
        {wakeOnRequest !== undefined && (
          <div>
            <Checkbox checked={chosenWakes} onChange={(e) => setWakes(e.target.checked)}>
              <Typography.Text strong>{t("sleepMode.wakeOnRequest")}</Typography.Text>
            </Checkbox>
            <div>
              <Typography.Text type="secondary">{chosenWakes ? t("sleepMode.wakeOnRequestOn") : t("sleepMode.wakeOnRequestOff")}</Typography.Text>
            </div>
          </div>
        )}
      </Space>
    </Modal>
  );
}
