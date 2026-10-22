import { Component, type ErrorInfo, type ReactNode } from "react";
import { Result, Button } from "antd";
import { withTranslation, type WithTranslation } from "react-i18next";

class AppErrorBoundary extends Component<{ children: ReactNode } & WithTranslation, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  componentDidCatch(error: Error, info: ErrorInfo) { console.error("UI render failed", error, info); }
  render() {
    if (this.state.failed) {
      const { t } = this.props;
      return <Result status="error" title={t("errorBoundary.title")} subTitle={t("errorBoundary.description")} extra={<Button type="primary" onClick={() => location.reload()}>{t("actions.refresh")}</Button>} />;
    }
    return this.props.children;
  }
}

export default withTranslation("common")(AppErrorBoundary);
