import { GlobalOutlined } from "@ant-design/icons";
import { Button, Dropdown, type MenuProps } from "antd";
import { useTranslation } from "react-i18next";

export default function LanguageSwitcher({ light = false }: { light?: boolean }) {
  const { t, i18n } = useTranslation();
  const current = i18n.resolvedLanguage === "zh" ? "zh" : "en";
  const items: MenuProps["items"] = [
    { key: "en", label: "English" },
    { key: "zh", label: String.fromCodePoint(0x7b80, 0x4f53, 0x4e2d, 0x6587) },
  ];
  const compactLabels = {
    en: "EN",
    zh: String.fromCodePoint(0x4e2d, 0x6587),
  };

  return (
    <Dropdown
      menu={{
        items,
        selectable: true,
        selectedKeys: [current],
        onClick: ({ key }) => void i18n.changeLanguage(key),
      }}
      placement="bottomRight"
      trigger={["click"]}
    >
      <Button
        type={light ? "text" : "default"}
        icon={<GlobalOutlined />}
        aria-label={t("language.switch")}
        title={t("language.switch")}
      >
        {compactLabels[current]}
      </Button>
    </Dropdown>
  );
}
