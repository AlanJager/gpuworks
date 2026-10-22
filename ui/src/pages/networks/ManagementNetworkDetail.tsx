import { Alert, Card, Space, Tabs } from "antd";
import { useTranslation } from "react-i18next";
import type { NetworkDetail } from "../../networkTypes";
import PageHeader from "../../components/PageHeader";
import { RoleTags, TypeTag } from "./networkDisplay";
import ManagementNodesTable from "./ManagementNodesTable";
import NetworkMonitorPanel from "./NetworkMonitorPanel";
import NetworkTestPanel from "./NetworkTestPanel";

/**
 * The management network cannot be edited, deleted or attached; what can
 * change is which roles it carries on each node, set here node by node or in
 * bulk, and on each node's Networks tab.
 */
export default function ManagementNetworkDetail({ detail, onChanged }: { detail: NetworkDetail; onChanged: () => void }) {
  const { t } = useTranslation("networks");
  const name = t("management.name");
  return (
    <>
      <PageHeader
        title={name}
        description={t("management.description")}
        breadcrumb={[t("breadcrumb.resources"), t("title"), name]}
        extra={<Space><RoleTags roles={detail.roles} /><TypeTag type={detail.type} /></Space>}
      />
      <Tabs
        items={[
          { key: "overview", label: t("tabs.overview"), children: (
            <>
              <Alert style={{ marginBottom: 16 }} type="info" showIcon message={t("fallbackNote")} />
              <Card title={t("detail.resolved", { count: detail.nodes.length })}>
                <ManagementNodesTable nodes={detail.nodes} onChanged={onChanged} />
              </Card>
            </>
          ) },
          { key: "test", label: t("tabs.test"), children: <NetworkTestPanel network={detail} onStarted={onChanged} onChanged={() => onChanged()} /> },
          { key: "monitor", label: t("tabs.monitor"), children: <NetworkMonitorPanel network={detail} /> },
        ]}
      />
    </>
  );
}
