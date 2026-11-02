import { createContext, useContext, type ReactNode } from "react";
import { ConfigProvider } from "antd";
import { LockOutlined } from "@ant-design/icons";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";

/**
 * How the page was opened: to create, to edit the deployment `deploymentID`
 * (its locked controls cannot change), or to clone it and modify the copy.
 */
export interface DeployEdit {
  mode: "create" | "edit" | "from";
  deploymentID?: number;
  /** The form controls the draft locks (see deployEditLocks). */
  locked: ReadonlySet<string>;
}

export const DeployEditContext = createContext<DeployEdit>({
  mode: "create",
  locked: new Set(),
});

/** Whether any of the named controls is locked for the deployment being edited. */
export function useControlLocked(...controls: string[]): boolean {
  const { locked } = useContext(DeployEditContext);
  return controls.some((control) => locked.has(control));
}

/**
 * LockedHint says a locked control cannot change in an edit, and that cloning
 * is how to change it. It renders nothing while the controls are editable.
 */
export function LockedHint({ controls }: { controls: string[] }) {
  const { t } = useTranslation("deploy");
  const { deploymentID } = useContext(DeployEditContext);
  const locked = useControlLocked(...controls);
  if (!locked) return null;
  return (
    <div className="deploy-locked-hint">
      <LockOutlined aria-hidden />
      <span>{t("edit.locked")}</span>
      <Link to={`/deploy?from=${deploymentID}`}>{t("edit.lockedClone")}</Link>
    </div>
  );
}

/**
 * LockedGroup disables the controls it wraps when an edit cannot change them,
 * and says why beneath: changing them is a clone's job. Unlocked, it renders
 * its children as they are. The native fieldset takes the plain inputs and
 * buttons out of reach, the config provider greys the antd controls.
 */
export function LockedGroup({
  controls,
  hint = true,
  children,
}: {
  controls: string[];
  hint?: boolean;
  children: ReactNode;
}) {
  const locked = useControlLocked(...controls);
  if (!locked) return <>{children}</>;
  return (
    <>
      <fieldset disabled className="deploy-locked-group">
        <ConfigProvider componentDisabled>{children}</ConfigProvider>
      </fieldset>
      {hint && <LockedHint controls={controls} />}
    </>
  );
}
