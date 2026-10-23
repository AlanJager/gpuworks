import type { ReactNode } from "react";
import { Alert, Button, Empty, Skeleton, Spin } from "antd";
import { useTranslation } from "react-i18next";
import { apiErrorText } from "../../apiError";
import type { AnalyticsData } from "./useAnalyticsData";

interface Props<T> {
  query: AnalyticsData<T>;
  isEmpty: (data: T) => boolean;
  /** Extra content under the error message, e.g. the way out of a refused query. */
  errorExtra?: (error: Error) => ReactNode;
  children: (data: T) => ReactNode;
}

// QueryState gives every tab the same four states: loading (a skeleton the
// first time, a veil over the previous answer after), error with retry, empty
// with a hint that statistics only accumulate from the upgrade on, and the
// content itself.
export default function QueryState<T>({ query, isEmpty, errorExtra, children }: Props<T>) {
  const { t } = useTranslation("analytics");
  const { t: tCommon } = useTranslation("common");
  const { data, error, loading, retry } = query;
  if (error) {
    return (
      <Alert
        type="error"
        showIcon
        message={t("state.loadFailed")}
        description={<>{apiErrorText(tCommon, error)}{errorExtra?.(error)}</>}
        action={<Button size="small" onClick={retry}>{tCommon("actions.retry")}</Button>}
      />
    );
  }
  if (!data) return loading ? <Skeleton active paragraph={{ rows: 8 }} /> : null;
  if (isEmpty(data)) {
    return (
      <Spin spinning={loading}>
        <Empty description={<><div>{t("state.empty")}</div><div style={{ fontSize: 12 }}>{t("state.emptyHint")}</div></>} />
      </Spin>
    );
  }
  return <Spin spinning={loading}>{children(data)}</Spin>;
}
