import { DownloadOutlined } from "@ant-design/icons";
import { Button, Dropdown } from "antd";
import { useTranslation } from "react-i18next";

export type BenchmarkReportFormat = "markdown" | "csv" | "html";

export function benchmarkReportURL(id: number, format: BenchmarkReportFormat) {
	return `/api/v1/benchmarks/${id}/report?format=${format}`;
}

export function benchmarkReportDownloads(id: number) {
	return (["markdown", "csv", "html"] as const).map((format) => ({ format, href: benchmarkReportURL(id, format) }));
}

export default function BenchmarkReportMenu({ runID }: { runID: number }) {
	const { t } = useTranslation("evaluations");
	return <Dropdown menu={{ items: benchmarkReportDownloads(runID).map(({ format, href }) => ({
		key: format,
		label: <a href={href} download>{t(`agentic.export.${format}`)}</a>,
	})) }} trigger={["click"]}>
		<Button icon={<DownloadOutlined />}>{t("agentic.export.button")}</Button>
	</Dropdown>;
}
