import { CheckOutlined, CopyOutlined } from "@ant-design/icons";
import { Button, Input, Modal, Tooltip, Typography, message } from "antd";
import type { ButtonProps } from "antd";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { copyText } from "../clipboard";

export interface CopyOptions {
  /** Toast shown after a successful copy; omit for silent success. */
  success?: string;
  /** Title of the manual-copy dialog shown when the browser refuses. */
  manualTitle?: string;
}

/**
 * useCopy copies through copyText (Clipboard API, then the plain-HTTP
 * selection fallback) and, when the browser refuses both, opens a dialog with
 * the text pre-selected so the user can press Ctrl+C. Render the returned
 * holder once in the calling component, like antd's message.useMessage.
 */
export function useCopy(): [(text: string, options?: CopyOptions) => Promise<boolean>, ReactNode] {
  const { t } = useTranslation("common");
  const [manual, setManual] = useState<{ text: string; title?: string }>();
  const copy = useCallback(async (text: string, options: CopyOptions = {}) => {
    if (await copyText(text)) {
      if (options.success) message.success(options.success);
      return true;
    }
    setManual({ text, title: options.manualTitle });
    return false;
  }, []);
  const holder = manual && (
    <Modal
      open
      title={manual.title || t("copy.manualTitle")}
      okText={t("copy.done")}
      cancelButtonProps={{ style: { display: "none" } }}
      onOk={() => setManual(undefined)}
      onCancel={() => setManual(undefined)}
    >
      <Typography.Paragraph>{t("copy.manualHint")}</Typography.Paragraph>
      <Input.TextArea readOnly autoFocus autoSize={{ minRows: 4, maxRows: 16 }} value={manual.text} onFocus={(event) => event.currentTarget.select()} />
    </Modal>
  );
  return [copy, holder];
}

const COPIED_FEEDBACK_MS = 2000;

interface CopyButtonProps {
  /** The text to copy, or a function evaluated at click time. */
  text: string | (() => string);
  /** Accessible name and tooltip. */
  label: string;
  success?: string;
  manualTitle?: string;
  size?: ButtonProps["size"];
  type?: ButtonProps["type"];
  disabled?: boolean;
  className?: string;
  children?: ReactNode;
}

/** CopyButton is an icon button that copies text and briefly shows a check mark. */
export default function CopyButton({ text, label, success, manualTitle, size, type = "text", disabled, className, children }: CopyButtonProps) {
  const { t } = useTranslation("common");
  const [copy, holder] = useCopy();
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>();
  useEffect(() => () => clearTimeout(timer.current), []);
  const onClick = async () => {
    if (!(await copy(typeof text === "function" ? text() : text, { success, manualTitle }))) return;
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), COPIED_FEEDBACK_MS);
  };
  return (
    <>
      <Tooltip title={copied ? t("copy.copied") : label}>
        <Button
          className={className}
          size={size}
          type={type}
          disabled={disabled}
          icon={copied ? <CheckOutlined /> : <CopyOutlined />}
          aria-label={label}
          onClick={() => void onClick()}
        >
          {children}
        </Button>
      </Tooltip>
      {holder}
    </>
  );
}
