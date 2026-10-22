import { LinkOutlined } from "@ant-design/icons";
import { Button, Modal, Tabs, Typography } from "antd";
import { Link } from "react-router-dom";
import type { PlaygroundModel } from "./types";
import { useTranslation } from "react-i18next";

function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'"'"'`)}'`;
}

function publicBody(body: Record<string, unknown>, model: PlaygroundModel): Record<string, unknown> {
  return { ...body, model: model.external ? model.id.split(":").slice(2).join(":") : model.id };
}

export default function ViewCodeModal({
  open,
  onClose,
  body,
  model,
}: {
  open: boolean;
  onClose: () => void;
  body: Record<string, unknown>;
  model: PlaygroundModel;
}) {
  const { t } = useTranslation("playground");
  const payload = publicBody(body, model);
  const pretty = JSON.stringify(payload, null, 2);
  // Examples use the address this page was opened from: the one the reader
  // already reaches the server on.
  const origin = window.location.origin;
  const curl = [
    `curl ${shellQuote(origin + "/v1/chat/completions")} \\`,
    `  -H "Authorization: Bearer <API Token>" \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -d ${shellQuote(pretty)}`,
  ].join("\n");
  const python = `import json
from openai import OpenAI

client = OpenAI(
    base_url=${JSON.stringify(origin + "/v1")},
    api_key="<API Token>",
)

payload = json.loads(${JSON.stringify(pretty)})
stream = client.chat.completions.create(**payload)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="", flush=True)`;

  return (
    <Modal title={t("code.title")} open={open} onCancel={onClose} footer={<Button onClick={onClose}>{t("code.close")}</Button>} width={760}>
      {!model.public_data_plane && (
        <Typography.Paragraph type="warning">
          {t("code.externalWarning")}
        </Typography.Paragraph>
      )}
      <Tabs items={[
        { key: "curl", label: "cURL", children: <pre className="playground-code">{curl}</pre> },
        { key: "python", label: "Python", children: <pre className="playground-code">{python}</pre> },
      ]} />
      <Link to="/apikeys" onClick={onClose}><LinkOutlined /> {t("code.manageKeys")}</Link>
    </Modal>
  );
}
