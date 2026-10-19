import { useEffect, useState } from "react";
import { Alert, Button, Modal, Spin } from "antd";
import { useTranslation } from "react-i18next";
import { api, type KubeCluster } from "../../api";
import CopyButton from "../../components/CopyButton";

/** The cluster's manifest, the YAML `kubectl apply` installs. Mount it while it is open. */
export default function KubeManifestModal({ cluster, onClose }: { cluster: KubeCluster; onClose: () => void }) {
  const { t } = useTranslation("kubernetes");
  const [yaml, setYaml] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    api.kubeManifest(cluster.uuid)
      .then((text) => { if (alive) setYaml(text); })
      .catch((e: Error) => { if (alive) setError(e.message); });
    return () => { alive = false; };
  }, [cluster.uuid]);
  return (
    <Modal
      open
      width={760}
      title={t("manifest.title", { name: cluster.name })}
      onCancel={onClose}
      footer={[
        yaml !== null && <CopyButton key="copy" type="default" label={t("manifest.copy")} text={yaml}>{t("manifest.copy")}</CopyButton>,
        <Button key="close" type="primary" onClick={onClose}>{t("manifest.close")}</Button>,
      ]}
    >
      {error !== null && <Alert type="error" showIcon message={t("manifest.failed")} description={error} />}
      {error === null && yaml === null && <Spin />}
      {yaml !== null && (
        <pre tabIndex={0} style={{ background: "#0d1117", color: "#e6edf3", padding: 16, borderRadius: 2, overflow: "auto", maxHeight: "60vh", margin: 0 }}>{yaml}</pre>
      )}
    </Modal>
  );
}
