import { Checkbox } from "../../components/ui/checkbox";
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
    <label className="flex items-center gap-2 px-1.5">
      <Checkbox checked={checked} onCheckedChange={(value) => onChange(value === true)} />
      {t("import.url.allowAuto", { host })}
    </label>
  );
}
