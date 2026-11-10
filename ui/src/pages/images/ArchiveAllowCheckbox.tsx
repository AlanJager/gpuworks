import { Checkbox } from "antd";
import { useTranslation } from "react-i18next";

type Props = {
  host: string;
  checked: boolean;
  onChange: (checked: boolean) => void;
};

/** Confirmation that resolving this address may write its host to the allow list. */
export default function ArchiveAllowCheckbox({ host, checked, onChange }: Props) {
  const { t } = useTranslation("images");
  if (!host) return null;
  return (
    <Checkbox checked={checked} onChange={(event) => onChange(event.target.checked)}>
      {t("import.url.allowAuto", { host })}
    </Checkbox>
  );
}
