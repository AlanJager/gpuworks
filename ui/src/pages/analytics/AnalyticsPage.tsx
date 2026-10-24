import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, Navigate, useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Skeleton, Tabs } from "antd";
import { Trans, useTranslation } from "react-i18next";
import { api, type DataRetention } from "../../api";
import type { Granularity } from "../../analyticsApi";
import PageHeader from "../../components/PageHeader";
import { downloadCsv, type CsvModel } from "./csv";
import { ExportContext } from "./exportContext";
import GpuTab from "./GpuTab";
import GpuTimeTab from "./GpuTimeTab";
import QualityTab from "./QualityTab";
import Toolbar, { type DeploymentOption } from "./Toolbar";
import { TAB_KEYS, isTabKey, type TabKey, type TabProps } from "./types";
import UsageTab from "./UsageTab";
import { compactDate, isQuickKey, quickRange, usableZone, type QuickKey } from "./zoned";

const DAY_MS = 86_400_000;
/** The longest range the server answers by hour. */
const MAX_HOUR_DAYS = 7;
/** Without a granularity, a range up to this long is read by hour. */
const AUTO_HOUR_MS = 2 * DAY_MS;
/** The parameters every tab shares; a tab's own are dropped when the tab changes. */
const SHARED_PARAMS = ["range", "from", "to", "gran", "deployment"];

function positiveInt(value: string | null): number | undefined {
  const n = Number(value);
  return value && Number.isInteger(n) && n > 0 ? n : undefined;
}

// AnalyticsPage is the usage analytics page: a time range, granularity
// and deployment shared by four tabs, all of it kept in the URL so a link or a
// refresh shows the same view.
export default function AnalyticsPage({ now = Date.now }: { now?: () => number }) {
  const { t } = useTranslation("analytics");
  const { tab: tabParam } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  const [search, setSearchParams] = useSearchParams();
  const tab: TabKey = isTabKey(tabParam) ? tabParam : "usage";

  const [retention, setRetention] = useState<DataRetention | null>(null);
  const [retentionSettled, setRetentionSettled] = useState(false);
  const [deployments, setDeployments] = useState<DeploymentOption[]>([]);
  const [anchor, setAnchor] = useState(() => now());
  const [reload, setReload] = useState(0);
  const [exportModel, setExportModel] = useState<CsvModel | null>(null);

  useEffect(() => {
    let alive = true;
    api.dataRetention()
      .then((view) => { if (alive) setRetention(view.value); })
      .catch(() => {})
      .finally(() => { if (alive) setRetentionSettled(true); });
    api.deployments()
      .then((list) => { if (alive) setDeployments(list.items.map((d) => ({ id: d.id, name: d.name })).sort((a, b) => a.name.localeCompare(b.name))); })
      .catch(() => {});
    return () => { alive = false; };
  }, []);

  // The URL's parameters are read and written through refs so setSearch keeps
  // one identity and two calls in a row do not undo each other.
  const searchRef = useRef(search);
  searchRef.current = search;
  const setParamsRef = useRef(setSearchParams);
  setParamsRef.current = setSearchParams;
  const setSearch = useCallback((patch: Record<string, string | string[] | undefined>) => {
    const next = new URLSearchParams(searchRef.current);
    for (const [key, value] of Object.entries(patch)) {
      next.delete(key);
      if (Array.isArray(value)) for (const v of value) next.append(key, v);
      else if (value !== undefined && value !== "") next.set(key, value);
    }
    searchRef.current = next;
    setParamsRef.current(next, { replace: true });
  }, []);

  const tz = usableZone(retention?.time_zone);
  const customFrom = positiveInt(search.get("from"));
  const customTo = positiveInt(search.get("to"));
  const hasCustom = customFrom !== undefined && customTo !== undefined && customTo > customFrom;
  const rangeParam = search.get("range");
  const range: QuickKey | "custom" = rangeParam === "custom" && hasCustom ? "custom" : isQuickKey(rangeParam) ? rangeParam : hasCustom ? "custom" : "today";
  const { from, to } = useMemo(
    () => (range === "custom" ? { from: customFrom!, to: customTo! } : quickRange(range, anchor, tz)),
    [range, customFrom, customTo, anchor, tz],
  );

  const hourDays = tab === "usage" ? retention?.usage_hourly_days : retention?.stats_hourly_days;
  const dailyDays = tab === "usage" ? retention?.usage_daily_days : retention?.stats_daily_days;
  const hourLimitDays = Math.min(MAX_HOUR_DAYS, hourDays ?? MAX_HOUR_DAYS);
  const span = to - from;
  const hourAllowed = span <= hourLimitDays * DAY_MS;
  const granParam = search.get("gran");
  const granularity: Granularity = !hourAllowed ? "day" : granParam === "hour" || granParam === "day" ? granParam : span <= AUTO_HOUR_MS ? "hour" : "day";

  useEffect(() => {
    if (tabParam !== undefined && !isTabKey(tabParam)) navigate(`/analytics/usage${location.search}`, { replace: true });
  }, [tabParam, navigate, location.search]);

  const deployment = positiveInt(search.get("deployment"));

  const exportRegistration = useCallback((model: CsvModel | null) => setExportModel(model), []);
  const exportNow = () => {
    if (exportModel) downloadCsv(exportModel, `ops-${exportModel.name}-${compactDate(from, tz)}_${compactDate(to, tz)}.csv`);
  };

  const changeTab = (key: string) => {
    const shared = new URLSearchParams();
    for (const name of SHARED_PARAMS) {
      const value = search.get(name);
      if (value) shared.set(name, value);
    }
    const query = shared.toString();
    navigate(`/analytics/${key}${query ? `?${query}` : ""}`);
  };

  if (tabParam === undefined) return <Navigate to={{ pathname: "/analytics/usage", search: location.search }} replace />;

  const props: TabProps = { from, to, granularity, deployment, tz, reload, search, setSearch };
  return (
    <div>
      <PageHeader title={t("title")} toolbar={
          <Toolbar
            range={range}
            from={from}
            to={to}
            tz={tz}
            granularity={granularity}
            hourAllowed={hourAllowed}
            retention={
              <Trans
                t={t}
                i18nKey="subtitle"
                values={{ hourly: hourDays ?? "—", daily: dailyDays ?? "—" }}
                components={{ settings: <Link to="/settings#data-retention" /> }}
              />
            }
            deployments={deployments}
            deployment={deployment}
            deploymentDisabled={tab === "gpus"}
            exportDisabled={!exportModel}
            onQuick={(key) => { setAnchor(now()); setSearch({ range: key, from: undefined, to: undefined }); }}
            onCustom={(a, b) => setSearch({ range: "custom", from: String(a), to: String(b) })}
            onGranularity={(g) => setSearch({ gran: g })}
            onDeployment={(id) => setSearch({ deployment: id === undefined ? undefined : String(id) })}
            onExport={exportNow}
            onRefresh={() => { setAnchor(now()); setReload((n) => n + 1); }}
          />
      } />
      <Tabs activeKey={tab} onChange={changeTab} items={TAB_KEYS.map((key) => ({ key, label: t(`tabs.${key}`) }))} />
      <ExportContext.Provider value={exportRegistration}>
        {!retentionSettled ? <Skeleton active paragraph={{ rows: 8 }} /> : (
          <>
            {tab === "usage" && <UsageTab {...props} />}
            {tab === "quality" && <QualityTab {...props} />}
            {tab === "gpus" && <GpuTab {...props} />}
            {tab === "gpu-time" && <GpuTimeTab {...props} />}
          </>
        )}
      </ExportContext.Provider>
    </div>
  );
}
