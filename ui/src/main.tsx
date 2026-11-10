import "@ant-design/v5-patch-for-react-19";
import React from "react";
import ReactDOM from "react-dom/client";
import { ConfigProvider, theme } from "antd";
import enUS from "antd/locale/en_US";
import zhCN from "antd/locale/zh_CN";
import { BrowserRouter } from "react-router-dom";
import { useTranslation } from "react-i18next";
import App from "./App";
import "./i18n";
import { themeConfig } from "./theme/tokens";
import "./styles.css";
import "./pages/evaluations/benchmarkReportExport.css";

function Root() {
  const { i18n } = useTranslation();
  const antdLocale = i18n.resolvedLanguage === "zh" ? zhCN : enUS;

  return (
    <ConfigProvider
      locale={antdLocale}
      theme={{ algorithm: theme.defaultAlgorithm, ...themeConfig }}
    >
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </ConfigProvider>
  );
}

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>,
);
