import { useMemo, useRef, useState } from "react";
import { Alert, Button, Drawer, Input, Space, Typography } from "antd";
import dayjs from "dayjs";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import { TRACE_ID_PATTERN, type MonitorRequestLookup, type MonitorRequestLookupQuery } from "../../monitorRequestsApi";
import { Placeholder, RequestList } from "./RequestView";

// The widest window a lookup asks for when the user widens it: the server's
// limit (uxrecord.MaxLookupWindow).
const WIDEST_MS = 24 * 60 * 60 * 1000;

interface State {
  query: MonitorRequestLookupQuery;
  loading: boolean;
  error: string;
  result: MonitorRequestLookup | null;
}

// RequestLookup finds requests across deployments by a trace id (every
// request of the trace) or a request id: the box sits in the monitor
// header, the answer opens in a drawer listing the requests, each opening
// its details.
export default function RequestLookup() {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const [text, setText] = useState("");
  const [state, setState] = useState<State | null>(null);
  const generation = useRef(0);

  const run = async (query: MonitorRequestLookupQuery) => {
    const mine = ++generation.current;
    setState({ query, loading: true, error: "", result: null });
    try {
      const result = await api.monitorRequestLookup(query);
      if (mine === generation.current) setState({ query, loading: false, error: "", result });
    } catch (e) {
      if (mine === generation.current) setState({ query, loading: false, error: apiErrorText(tCommon, e), result: null });
    }
  };

  const search = (raw: string) => {
    const value = raw.trim();
    if (!value) return;
    void run(TRACE_ID_PATTERN.test(value) ? { traceId: value } : { requestId: value });
  };

  const names = useMemo(() => new Map((state?.result?.deployments ?? []).map((d) => [d.id, d.name])), [state?.result]);
  const result = state?.result;
  const kind = state?.query.traceId ? "trace" : "request";
  const widen = () => {
    if (!state) return;
    const now = Date.now();
    void run({ ...state.query, from: now - WIDEST_MS, to: now });
  };

  return (
    <>
      <Input.Search
        size="small"
        allowClear
        value={text}
        onChange={(e) => setText(e.target.value)}
        onSearch={search}
        placeholder={t("lookup.placeholder")}
        aria-label={t("lookup.label")}
        style={{ width: 200 }}
      />
      <Drawer open={state != null} onClose={() => setState(null)} title={t(`lookup.title.${kind}`)} width={720}>
        {state && (
          <Space direction="vertical" size={8} style={{ width: "100%" }}>
            <Typography.Text code copyable>
              {state.query.traceId ?? state.query.requestId}
            </Typography.Text>
            {state.error && (
              <Alert type="error" showIcon message={t("lookup.failed")} description={state.error} action={<Button size="small" onClick={() => void run(state.query)}>{tCommon("actions.retry")}</Button>} />
            )}
            {state.loading && <Placeholder text={t("lookup.searching")} />}
            {result && (
              <>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {t(`lookup.window.${result.window}`, { from: dayjs(result.from_ms).format("MM-DD HH:mm"), to: dayjs(result.to_ms).format("MM-DD HH:mm") })}
                  {result.requests.length > 0 && ` · ${t("lookup.found", { count: result.requests.length })}`}
                </Typography.Text>
                {result.requests.length > 0 ? (
                  <RequestList requests={result.requests} label={t("lookup.results")} replicas={new Map()} deploymentNames={names} />
                ) : (
                  <Alert
                    type="info"
                    showIcon
                    message={t("lookup.none")}
                    description={t("lookup.noneHelp")}
                    action={
                      result.to_ms - result.from_ms < WIDEST_MS ? (
                        <Button size="small" onClick={widen}>
                          {t("lookup.widen")}
                        </Button>
                      ) : undefined
                    }
                  />
                )}
              </>
            )}
          </Space>
        )}
      </Drawer>
    </>
  );
}
