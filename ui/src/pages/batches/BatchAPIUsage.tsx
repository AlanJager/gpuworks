import { Collapse, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";

/**
 * batchAPISteps are the OpenAI-compatible calls a client makes to run a batch
 * through the gateway, with the address the console was opened from.
 */
export function batchAPISteps(gateway: string): { key: string; code: string }[] {
  return [
    {
      key: "upload",
      code: `curl ${gateway}/v1/files \\
  -H "Authorization: Bearer <key>" \\
  -F purpose=batch -F file=@requests.jsonl`,
    },
    {
      key: "create",
      code: `curl ${gateway}/v1/batches \\
  -H "Authorization: Bearer <key>" -H "Content-Type: application/json" \\
  -d '{"input_file_id":"<file id>","endpoint":"/v1/chat/completions","completion_window":"24h"}'`,
    },
    {
      key: "poll",
      code: `curl ${gateway}/v1/batches/<batch id> -H "Authorization: Bearer <key>"`,
    },
    {
      key: "download",
      code: `curl ${gateway}/v1/files/<output_file_id>/content -H "Authorization: Bearer <key>" -o results.jsonl`,
    },
    {
      key: "sdk",
      code: `from openai import OpenAI
client = OpenAI(base_url="${gateway}/v1", api_key="<key>")
f = client.files.create(file=open("requests.jsonl", "rb"), purpose="batch")
b = client.batches.create(input_file_id=f.id, endpoint="/v1/chat/completions", completion_window="24h")
print(client.batches.retrieve(b.id).status)`,
    },
  ];
}

/**
 * BatchAPIUsage tells the reader batches are an API first: the same
 * OpenAI-compatible /v1/files and /v1/batches any client calls with an API
 * key, of which this page is one view.
 */
export default function BatchAPIUsage() {
  const { t } = useTranslation("batches");
  const gateway = window.location.origin;
  return (
    <Collapse
      className="batch-api-usage"
      items={[{
        key: "api",
        label: t("api.title"),
        children: (
          <div className="batch-api-steps">
            <Typography.Paragraph type="secondary">
              {t("api.description")} <Link to="/apikeys">{t("api.keys")}</Link>
            </Typography.Paragraph>
            {batchAPISteps(gateway).map((step) => (
              <div key={step.key}>
                <Typography.Text strong>{t(`api.steps.${step.key}`)}</Typography.Text>
                <pre className="code-block">{step.code}</pre>
              </div>
            ))}
          </div>
        ),
      }]}
    />
  );
}
