import type { ReactElement } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "../../components/ui/button";
import { DropdownContent, DropdownMenu, DropdownSeparator, DropdownTrigger } from "../../components/ui/dropdown";
import { MenuItem } from "../../components/ui/menu-item";
import { Tooltip } from "../../components/ui/tooltip";
import type { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { ConsoleIcon as Icons } from "../../ui-kit/consoleIcons";

/** One action of a row. */
export interface RowAction {
  key: string;
  label: string;
  icon: (typeof ConsoleIcon)[keyof typeof ConsoleIcon];
  onSelect: () => void;
  disabled?: boolean;
  /** Why it is disabled, said on hover. */
  hint?: string;
  /** Destructive: drawn red, and listed last under a rule when folded. */
  danger?: boolean;
  /** Kept beside the menu when the row's actions are folded. */
  common?: boolean;
  loading?: boolean;
  /** The row's main next step: drawn as the primary button when the row's buttons carry their names (labeled). */
  primary?: boolean;
}

// A row shows this many icon buttons before the rest fold into a menu.
const SHOWN = 3;

/**
 * A row's actions as icon buttons, each with its name on hover. A row with
 * more than three keeps its common ones out and folds the rest into a "More"
 * menu, the destructive ones last. Confirming an action is the action's own
 * concern: from a menu it has to be a dialog, since the menu closes.
 *
 * `labeled` writes each button's name beside its icon, and the "More" button's
 * too, for a header whose actions a reader should not have to hover to
 * learn; the main step (`primary`) is the filled button. The names stay the
 * buttons' accessible names either way.
 */
export default function RowActions({ actions, label, labeled = false }: { actions: RowAction[]; label: string; labeled?: boolean }) {
  const { t } = useTranslation("common");
  const fold = actions.length > SHOWN;
  const shown = fold ? actions.filter((a) => a.common) : actions;
  const folded = fold ? actions.filter((a) => !a.common) : [];
  const plain = folded.filter((a) => !a.danger);
  const danger = folded.filter((a) => a.danger);
  const button = (a: RowAction): ReactElement => (
    <Tooltip key={a.key} content={a.disabled && a.hint ? a.hint : a.label}>
      <span className="inline-flex">
        {labeled ? (
          // The Button lays its icon and its label out in a row itself; an icon placed among the children is not part of that row.
          <Button variant={a.primary ? "primary" : "secondary"} size="sm" leadingIcon={a.icon} aria-label={a.label} disabled={a.disabled} loading={a.loading} onClick={a.onSelect}>
            {a.label}
          </Button>
        ) : (
          <Button variant="ghost" size="sm" iconOnly aria-label={a.label} disabled={a.disabled} loading={a.loading}
            className={a.danger ? "text-fg-danger hover:text-fg-danger" : undefined} onClick={a.onSelect}>
            <a.icon size={16} />
          </Button>
        )}
      </span>
    </Tooltip>
  );
  const item = (a: RowAction, index: number) => (
    <MenuItem key={a.key} index={index} icon={a.icon} label={a.label} disabled={a.disabled} title={a.disabled ? a.hint : undefined}
      className={a.danger ? "text-fg-danger [&_*]:text-fg-danger" : undefined} onSelect={a.onSelect} />
  );
  return (
    <div className={labeled ? "flex items-center gap-1.5" : "flex items-center gap-0.5"} role="group" aria-label={label}>
      {shown.map(button)}
      {folded.length > 0 && (
        <DropdownMenu>
          <Tooltip content={t("actions.more")}>
            <span className="inline-flex">
              <DropdownTrigger render={labeled
                ? <Button variant="ghost" size="sm" leadingIcon={Icons.more} aria-label={t("actions.more")}>{t("actions.more")}</Button>
                : <Button variant="ghost" size="sm" iconOnly aria-label={t("actions.more")}><Icons.more size={16} /></Button>} />
            </span>
          </Tooltip>
          <DropdownContent align="end">
            {plain.map((a, i) => item(a, i))}
            {plain.length > 0 && danger.length > 0 && <DropdownSeparator />}
            {danger.map((a, i) => item(a, plain.length + i))}
          </DropdownContent>
        </DropdownMenu>
      )}
    </div>
  );
}
