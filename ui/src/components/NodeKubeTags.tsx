import { Tag, Tooltip } from "antd";
import { useTranslation } from "react-i18next";
import type { Node } from "../api";

/**
 * Where a node sits in Kubernetes: the cluster and the Kubernetes node name,
 * and a warning while Kubernetes has the node cordoned. A node that did not
 * join from a cluster shows nothing. A cluster the page does not know (not
 * loaded, or not visible to the viewer) leaves the node name alone.
 */
export default function NodeKubeTags({ node, clusterNames }: { node: Pick<Node, "kube">; clusterNames?: ReadonlyMap<number, string> }) {
  const { t } = useTranslation("nodes");
  const kube = node.kube;
  if (!kube) return null;
  const cluster = clusterNames?.get(kube.cluster_id);
  const place = cluster ? `${cluster}/${kube.node_name}` : kube.node_name;
  return (
    <>
      <Tag color="geekblue" style={{ marginInlineEnd: 4 }}>{t("kube.tag", { place })}</Tag>
      {kube.unschedulable && (
        <Tooltip title={t("kube.cordonedHint")}>
          <Tag color="warning" style={{ marginInlineEnd: 4 }}>{t("kube.cordoned")}</Tag>
        </Tooltip>
      )}
    </>
  );
}
