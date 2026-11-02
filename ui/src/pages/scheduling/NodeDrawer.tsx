import { Alert, Button, Drawer, Spin } from "antd";
import { useTranslation } from "react-i18next";
import { usePolling } from "../../api";
import { schedulingApi } from "../../schedulingApi";
import { Legend, NodeList } from "./GpuBoard";
import ResidencyTimeline from "./ResidencyTimeline";

const NODE_POLL_MS = 10_000;

/**
 * NodeDrawer is one node of the heatmap in detail: each GPU as a bar of
 * awake, sleeping and free memory with the deployments holding it, and the
 * node's residency timeline. Only now is the node read from the server in full;
 * the heatmap itself carries no occupant detail.
 */
export default function NodeDrawer({ uuid, name, onClose }: { uuid: string | null; name: string; onClose: () => void }) {
  const { t } = useTranslation("scheduling");
  return (
    <Drawer open={uuid != null} onClose={onClose} width={820} title={name} destroyOnClose aria-label={t("drawer.aria", { node: name })}>
      {uuid != null && <NodeDetail key={uuid} uuid={uuid} />}
    </Drawer>
  );
}

function NodeDetail({ uuid }: { uuid: string }) {
  const { t } = useTranslation("scheduling");
  const board = usePolling(() => schedulingApi.board({ node: uuid }), NODE_POLL_MS, [uuid]);
  const node = board.data?.nodes.items[0];
  if (!node && board.error) {
    return (
      <Alert
        type="error"
        showIcon
        message={t("drawer.loadFailed")}
        description={board.error.message}
        action={<Button onClick={() => void board.refetch()}>{t("page.retry")}</Button>}
      />
    );
  }
  if (!node) {
    return <div role="status" className="scheduling-loading"><Spin /> <span>{t("drawer.loading")}</span></div>;
  }
  return (
    <>
      {board.error && (
        <Alert
          style={{ marginBottom: 12 }}
          type="warning"
          showIcon
          message={t("page.refreshFailed", { detail: board.error.message })}
          action={<Button size="small" onClick={() => void board.refetch()}>{t("page.retry")}</Button>}
        />
      )}
      <Legend />
      <NodeList nodes={[node]} />
      <ResidencyTimeline nodes={[node]} />
    </>
  );
}
