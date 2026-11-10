import { DownOutlined, LockOutlined, LogoutOutlined, UserOutlined } from "@ant-design/icons";
import { Button, Dropdown, message, type MenuProps } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type AuthUser } from "../api";
import ChangePasswordModal from "./ChangePasswordModal";

/** UserMenu is the header's account entry: who is signed in, and what they can do with their own account. */
export default function UserMenu({ onLogout }: { onLogout: () => void }) {
  const { t } = useTranslation();
  const { t: tAuth } = useTranslation("auth");
  const [user, setUser] = useState<AuthUser>();
  const [changing, setChanging] = useState(false);

  useEffect(() => {
    let alive = true;
    api.me().then((me) => { if (alive) setUser(me); }).catch(() => {});
    return () => { alive = false; };
  }, []);

  const items: MenuProps["items"] = [
    { key: "password", icon: <LockOutlined />, label: t("header.changePassword") },
    { type: "divider" },
    { key: "logout", icon: <LogoutOutlined />, label: t("header.logout") },
  ];
  const name = user?.username ?? t("header.administrator");

  return (
    <>
      <Dropdown
        menu={{ items, onClick: ({ key }) => key === "password" ? setChanging(true) : onLogout() }}
        placement="bottomRight"
        trigger={["click"]}
      >
        <Button type="text" icon={<UserOutlined />} aria-label={t("header.account", { name })}>
          <span className="app-header-user">{name}</span>
          <DownOutlined className="app-header-user" style={{ fontSize: 10 }} />
        </Button>
      </Dropdown>
      <ChangePasswordModal
        open={changing}
        policy={user?.password_policy}
        onClose={() => setChanging(false)}
        onChanged={() => {
          setChanging(false);
          void message.success(tAuth("changePassword.done"));
          onLogout();
        }}
      />
    </>
  );
}
