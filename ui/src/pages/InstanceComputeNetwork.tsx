import { Space, Tag, Typography } from "antd";
import { networkDetailPath } from "./networks/networkLinks";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { InstanceComputeNetwork } from "../api";
import { Endpoint, TypeTag } from "./networks/networkDisplay";

/**
 * The network a multi-node rank started on. It is what the rank was launched
 * with, so it stays true after networks or management roles change.
 */
export default function InstanceComputeNetworkView({ network }: { network: InstanceComputeNetwork }) {
  const { t } = useTranslation("deployments");
  const { t: tNetworks } = useTranslation("networks");
  return (
    <Space size={6} wrap>
      {network.management ? (
        <>
          <Link to={networkDetailPath(0)}>{tNetworks("management.name")}</Link>
          <Tag color="orange">{t("instance.computeNetworkFallback")}</Tag>
        </>
      ) : (
        <Typography.Text strong>{network.name}</Typography.Text>
      )}
      <TypeTag type={network.type} />
      {network.link_layer && <Tag>{tNetworks(`linkLayer.${network.link_layer}`, { defaultValue: network.link_layer })}</Tag>}
      <Endpoint iface={network.interface} ip={network.ip} />
    </Space>
  );
}
