import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";
import { Button } from "./ui/button";
import { DropdownContent, DropdownMenu, DropdownSeparator, DropdownTrigger } from "./ui/dropdown";
import { MenuItem } from "./ui/menu-item";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { message } from "../ui-kit/feedback";
import ChangePasswordModal from "./ChangePasswordModal";
import { useSessionUser } from "./sessionUser";
import { useAccountMenuItems } from "../extensions/shell";
import type { AccountMenuEntry } from "../extensions";

const AccountIcon = ConsoleIcon.account;
const ChevronIcon = ConsoleIcon.chevronDown;

/**
 * UserMenu is the header's account entry: who is signed in, and what they can
 * do with their own account. Console extensions' account entries follow the
 * built-in ones, before signing out.
 */
export default function UserMenu({ onLogout }: { onLogout: () => void }) {
  const { t } = useTranslation();
  const { t: tAuth } = useTranslation("auth");
  const user = useSessionUser();
  const [changing, setChanging] = useState(false);
  const extensionEntries = useAccountMenuItems();

  const name = user?.username ?? t("header.administrator");
  return (
    <>
      <DropdownMenu>
        <DropdownTrigger
          render={(
            <Button variant="ghost" size="md" leadingIcon={AccountIcon} trailingIcon={ChevronIcon} aria-label={t("header.account", { name })}>
              <span className="app-header-user">{name}</span>
            </Button>
          )}
        />
        <DropdownContent align="end">
          <MenuItem index={0} label={t("header.changePassword")} onSelect={() => setChanging(true)} />
          {extensionEntries.length > 0 && <ExtensionEntries entries={extensionEntries} firstIndex={1} />}
          <DropdownSeparator />
          <MenuItem index={1 + extensionEntries.length} label={t("header.logout")} onSelect={onLogout} />
        </DropdownContent>
      </DropdownMenu>
      <ChangePasswordModal
        open={changing}
        policy={user?.password_policy}
        onClose={() => setChanging(false)}
        onChanged={() => {
          setChanging(false);
          message.success(tAuth("changePassword.done"));
          onLogout();
        }}
      />
    </>
  );
}

// ExtensionEntries are the extensions' account entries; each opens its console
// path. Kept apart so the menu needs a router only when an extension adds one.
function ExtensionEntries({ entries, firstIndex }: { entries: readonly AccountMenuEntry[]; firstIndex: number }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  return (
    <>
      {entries.map((entry, offset) => (
        <MenuItem key={entry.key} index={firstIndex + offset} label={t(entry.labelKey)} onSelect={() => navigate(entry.key)} />
      ))}
    </>
  );
}
