import { UploadOutlined } from "@ant-design/icons";
import { Button, Form, Input, Radio, Space, Typography, Upload } from "antd";
import { useTranslation } from "react-i18next";
import type { TokenizerSource } from "./tokenizerSource";
import { tokenizerReferenceName } from "./tokenizerSource";

export default function TokenizerSourceFields({ source, dataset, targetKind, tokenizerRef, onUpload }: {
	source: TokenizerSource;
	dataset: string;
	targetKind: "instance" | "external";
	tokenizerRef?: string;
	onUpload: (file: File) => void;
}) {
	const { t } = useTranslation("evaluations");
	const agentic = dataset === "agentic";
	const currentFile = tokenizerReferenceName(tokenizerRef);
	// The automatic source reads the target machine, so it only exists where
	// there is one. Agentic workloads cannot estimate, so estimation is offered
	// only where it can actually be used.
	const automatic = source === "auto";
	const helpKey = automatic ? "wizard.tokenizerAutoHelp" : agentic ? "wizard.tokenizerAgenticHelp" : targetKind === "instance" ? "wizard.tokenizerManagedHelp" : "wizard.tokenizerHelp";
	const repository = source === "huggingface" || source === "modelscope";

	return <>
		<Form.Item name="tokenizer_source" label={t("wizard.tokenizerSource")} extra={t(helpKey)}>
			<Radio.Group options={[
				{ value: "auto", label: t("wizard.tokenizerAuto"), disabled: targetKind === "external" },
				{ value: "estimate", label: t("wizard.tokenizerEstimate"), disabled: agentic },
				{ value: "huggingface", label: "Hugging Face" },
				{ value: "modelscope", label: "ModelScope" },
				{ value: "upload", label: t("wizard.uploadFile") },
			]} />
		</Form.Item>
		<Form.Item name="tokenizer_ref" hidden rules={source === "upload" ? [{ required: agentic, message: t("wizard.tokenizerRequired") }] : undefined}><Input /></Form.Item>
		{source === "upload" ? <Space direction="vertical" size={8}>
			{currentFile && <Typography.Text type="secondary">{t("wizard.currentTokenizerFile", { name: currentFile })}</Typography.Text>}
			<Upload accept=".json" showUploadList={false} beforeUpload={(file) => { onUpload(file); return false; }}>
				<Button icon={<UploadOutlined />}>{t("wizard.uploadTokenizer")}</Button>
			</Upload>
		</Space> : repository ? <Form.Item name="tokenizer_repo" label={t("wizard.modelRepository")} rules={targetKind === "external" ? [{ required: true, message: t("wizard.tokenizerRepositoryRequired") }] : undefined}>
			<Input placeholder={t("wizard.modelRepositoryPlaceholder")} />
		</Form.Item> : null}
	</>;
}
