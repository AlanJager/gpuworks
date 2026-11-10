import { Checkbox } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { outboundPolicyApi } from "../../outboundPolicyApi";

type Props = {
  host: string;
  checked: boolean;
  onChange: (checked: boolean) => void;
};

/**
 * Confirmation that resolving this address may write its host to the allow list.
 * Hidden unless private-host checks are on; with them off, ordinary private
 * hosts are already permitted and the list is not consulted.
 */
export default function ArchiveAllowCheckbox({ host, checked, onChange }: Props) {
  const { t } = useTranslation("images");
  const [restricted, setRestricted] = useState(false);
  useEffect(() => {
    let alive = true;
    outboundPolicyApi.get()
      .then((policy) => { if (alive) setRestricted(policy.restrict_private_hosts); })
      .catch(() => { if (alive) setRestricted(false); });
    return () => { alive = false; };
  }, []);
  if (!host || !restricted) return null;
  return (
    <Checkbox checked={checked} onChange={(event) => onChange(event.target.checked)}>
      {t("import.url.allowAuto", { host })}
    </Checkbox>
  );
}
