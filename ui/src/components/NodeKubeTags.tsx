import { useTranslation } from "react-i18next";
import type { Node } from "../api";
import { Badge } from "./ui/badge";
import { Tooltip } from "./ui/tooltip";

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
      <Badge color="indigo" className="me-1">{t("kube.tag", { place })}</Badge>
      {kube.unschedulable && (
        <Tooltip content={t("kube.cordonedHint")}>
          <Badge status="warning" className="me-1">{t("kube.cordoned")}</Badge>
        </Tooltip>
      )}
    </>
  );
}
