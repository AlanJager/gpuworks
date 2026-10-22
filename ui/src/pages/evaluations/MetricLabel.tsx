import { QuestionCircleOutlined } from "@ant-design/icons";
import { Space, Tooltip } from "antd";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { colors } from "../../theme/tokens";

// Metrics that carry a one-line definition. The wording lives in the
// evaluations locale under metricHelp.<key>.
export type MetricHelpKey = "pareto" | "output" | "total" | "request" | "interactivity" | "ttft" | "tpot" | "latency" | "maxOutput" | "maxTotal" | "success" | "requestRate" | "concurrency";

// MetricLabel renders a metric name followed by a help icon whose tooltip
// gives the metric's definition.
export default function MetricLabel({ label, help }: { label: ReactNode; help: MetricHelpKey }) {
	const { t } = useTranslation("evaluations");
	const definition = t(`metricHelp.${help}`);
	return <Space size={4} style={{ whiteSpace: "nowrap" }}>
		<span>{label}</span>
		<Tooltip title={definition}><QuestionCircleOutlined aria-label={definition} style={{ color: colors.muted, cursor: "help" }} /></Tooltip>
	</Space>;
}
