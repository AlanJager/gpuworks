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
	const tokenLengthControlled = dataset === "agentic" || dataset === "random";
	const currentFile = tokenizerReferenceName(tokenizerRef);
	// The automatic source asks the target's tokenize endpoint first; an
	// external endpoint has no machine or repository behind it, so there it is
	// that endpoint alone. Token-length workloads cannot estimate, so estimation
	// is offered only where it can actually be used.
	const automatic = source === "auto";
	const helpKey = automatic ? (targetKind === "instance" ? "wizard.tokenizerAutoHelp" : "wizard.tokenizerAutoExternalHelp") : source === "engine" ? "wizard.tokenizerEngineHelp" : tokenLengthControlled ? "wizard.tokenizerControlledHelp" : targetKind === "instance" ? "wizard.tokenizerManagedHelp" : "wizard.tokenizerHelp";
	const repository = source === "huggingface" || source === "modelscope";

	return <>
		<Form.Item name="tokenizer_source" label={t("wizard.tokenizerSource")} extra={t(helpKey)}>
			<Radio.Group options={[
				{ value: "auto", label: t("wizard.tokenizerAuto") },
				{ value: "engine", label: t("wizard.tokenizerEngine") },
				{ value: "estimate", label: t("wizard.tokenizerEstimate"), disabled: tokenLengthControlled },
				{ value: "huggingface", label: "Hugging Face" },
				{ value: "modelscope", label: "ModelScope" },
				{ value: "upload", label: t("wizard.uploadFile") },
			]} />
		</Form.Item>
		<Form.Item name="tokenizer_ref" hidden={source !== "upload"} style={{ marginBottom: 0 }} rules={source === "upload" ? [{ required: true, whitespace: true, message: t("wizard.tokenizerRequired") }] : undefined}><Input style={{ display: "none" }} /></Form.Item>
		{source === "upload" ? <Space direction="vertical" size={8}>
			{currentFile && <Typography.Text type="secondary">{t("wizard.currentTokenizerFile", { name: currentFile })}</Typography.Text>}
			<Upload accept=".json,.model,.tiktoken" showUploadList={false} beforeUpload={(file) => { onUpload(file); return false; }}>
				<Button icon={<UploadOutlined />}>{t("wizard.uploadTokenizer")}</Button>
			</Upload>
		</Space> : repository ? <Form.Item name="tokenizer_repo" label={t("wizard.modelRepository")} rules={[{ required: true, whitespace: true, message: t("wizard.tokenizerRepositoryRequired") }]}>
			<Input placeholder={t("wizard.modelRepositoryPlaceholder")} />
		</Form.Item> : null}
	</>;
}
