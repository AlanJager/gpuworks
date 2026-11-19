import { forwardRef, useMemo, useState, type AriaAttributes, type CSSProperties, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { api, usePolling, type Node, type NodeFilter } from "../api";
import { nodeDisplayName } from "../nodeDisplay";
import { useNodeLookup } from "./useNodeLookup";
import { Combobox, ComboboxChip, ComboboxChips, ComboboxChipsInput, ComboboxClear, ComboboxContent, ComboboxEmpty, ComboboxInput, ComboboxItem, ComboboxList, ComboboxValue, useComboboxAnchor } from "../components/ui/combobox";
import { Spinner } from "../ui-kit/Spinner";

/** How many nodes one search offers; typing narrows the list on the server. */
export const NODE_PICKER_PAGE_SIZE = 50;

type Value = string | string[] | undefined;

type Props = {
  value?: Value;
  onChange?: (value: Value, nodes: Node[]) => void;
  mode?: "multiple";
  filter?: NodeFilter;
  optionLabel?: (node: Node) => string;
  optionDisabled?: (node: Node) => boolean;
  optionTitle?: (node: Node) => string;
  unknownLabel?: (uuid: string) => string;
  placeholder?: string;
  disabled?: boolean;
  allowClear?: boolean;
  style?: CSSProperties;
  notFoundContent?: ReactNode;
  maxCount?: number;
  size?: "small" | "middle" | "large";
  "aria-label"?: string;
  id?: string;
} & Pick<AriaAttributes, "aria-invalid" | "aria-describedby" | "aria-required">;

type Option = { value: string; label: string; disabled?: boolean; title?: string };

const CONTROL_SIZE = { small: "sm", middle: "md", large: "lg" } as const;

/**
 * NodePicker selects nodes by searching them on the server (GET /nodes with
 * page parameters) instead of listing the fleet: it offers one page of the
 * nodes `filter` matches, narrowed by what the user types. Chosen nodes keep
 * their names when a later search leaves them out, and a chosen uuid that no
 * longer names a node is still shown (unknownLabel) so it can be removed.
 *
 * It works as a form control (value/onChange, id and aria attributes on its
 * input; the ref is the input, so a form can focus it).
 */
const NodePicker = forwardRef<HTMLInputElement, Props>(function NodePicker({
  value,
  onChange,
  mode,
  filter = {},
  optionLabel = nodeDisplayName,
  optionDisabled,
  optionTitle,
  unknownLabel = (uuid) => uuid,
  placeholder,
  disabled,
  allowClear,
  style,
  notFoundContent,
  maxCount,
  size,
  "aria-label": ariaLabel,
  "aria-invalid": ariaInvalid,
  "aria-describedby": ariaDescribedBy,
  "aria-required": ariaRequired,
  id,
}, ref) {
  const { t } = useTranslation("common");
  const [search, setSearch] = useState("");
  const anchor = useComboboxAnchor();
  const filterKey = JSON.stringify([filter.state ?? [], filter.label ?? [], filter.q ?? ""]);
  const page = usePolling(
    () => api.nodesPage({ page: 1, page_size: NODE_PICKER_PAGE_SIZE, q: search || filter.q, state: filter.state, label: filter.label }),
    0,
    [search, filterKey],
  );
  const selectedKey = JSON.stringify(value === undefined ? [] : Array.isArray(value) ? value : [value]);
  const selected = useMemo(() => JSON.parse(selectedKey) as string[], [selectedKey]);
  const items = page.data?.items;
  const known = useNodeLookup(selected, useMemo(() => items ?? [], [items]));
  const options = useMemo(() => {
    const listed = new Set<string>();
    const out: Option[] = [];
    for (const node of items ?? []) {
      listed.add(node.uuid);
      out.push({ value: node.uuid, label: optionLabel(node), disabled: optionDisabled?.(node), title: optionTitle?.(node) });
    }
    for (const uuid of selected) {
      if (listed.has(uuid)) continue;
      const node = known.get(uuid);
      out.unshift({ value: uuid, label: node ? optionLabel(node) : unknownLabel(uuid) });
    }
    return out;
  }, [items, known, selectedKey, optionLabel, optionDisabled, optionTitle, unknownLabel]);
  const byValue = useMemo(() => new Map(options.map((option) => [option.value, option])), [options]);
  const labelOf = (uuid: string) => byValue.get(uuid)?.label ?? unknownLabel(uuid);
  const emit = (uuids: string[], next: Value) =>
    onChange?.(next, uuids.map((uuid) => known.get(uuid) ?? items?.find((node) => node.uuid === uuid)).filter((node): node is Node => !!node));
  // Past maxCount the unchosen nodes cannot be added; chosen ones can still be removed.
  const full = mode === "multiple" && maxCount != null && selected.length >= maxCount;
  const aria = { id, "aria-label": ariaLabel, "aria-invalid": ariaInvalid, "aria-describedby": ariaDescribedBy, "aria-required": ariaRequired };
  const list = <ComboboxContent anchor={mode === "multiple" ? anchor : undefined}>
    {/* Typing searches the server, so nothing filters here; an empty page says why. */}
    <ComboboxEmpty>{page.loading ? <Spinner label={t("picker.loading")} /> : notFoundContent ?? t("dataTable.empty")}</ComboboxEmpty>
    {/* A new search over listed nodes keeps them shown and says it is still asking. */}
    {page.loading && options.length > 0 && <div className="px-3 py-1.5 text-body" data-slot="node-picker-loading"><Spinner label={t("picker.loading")} /></div>}
    <ComboboxList>{(uuid: string) => {
      const option = byValue.get(uuid);
      return <ComboboxItem key={uuid} value={uuid} title={option?.title || undefined} disabled={option?.disabled || (full && !selected.includes(uuid))}>{labelOf(uuid)}</ComboboxItem>;
    }}</ComboboxList>
  </ComboboxContent>;
  const common = {
    items: options.map((option) => option.value),
    filter: null,
    disabled,
    size: CONTROL_SIZE[size ?? "middle"],
    itemToStringLabel: labelOf,
    onInputValueChange: (text: string, details: { reason: string }) => { if (details.reason === "input-change") setSearch(text); },
    onOpenChange: (open: boolean) => { if (!open) setSearch(""); },
  };
  return (
    <div className="node-picker min-w-0" style={style}>
      {mode === "multiple" ? (
        <Combobox<string, true> {...common} multiple value={selected} onValueChange={(next) => emit(next, next)}>
          <ComboboxChips ref={anchor} className="w-full min-w-0">
            <ComboboxValue>{(chosen: string[]) => <>
              {chosen.map((uuid) => <ComboboxChip key={uuid} removeAriaLabel={t("picker.remove", { name: labelOf(uuid) })}>{labelOf(uuid)}</ComboboxChip>)}
              <ComboboxChipsInput ref={ref} {...aria} placeholder={chosen.length ? undefined : placeholder} />
              {allowClear && chosen.length > 0 && <ComboboxClear aria-label={t("picker.clear")} />}
            </>}</ComboboxValue>
          </ComboboxChips>
          {list}
        </Combobox>
      ) : (
        <Combobox<string> {...common} value={selected[0] ?? null} onValueChange={(next) => emit(next == null ? [] : [next], next ?? undefined)}>
          <ComboboxInput ref={ref} {...aria} className="w-full" placeholder={placeholder} disabled={disabled}
            showClear={!!allowClear && selected.length > 0} clearAriaLabel={t("picker.clear")} triggerAriaLabel={t("picker.open")} />
          {list}
        </Combobox>
      )}
    </div>
  );
});

export default NodePicker;
