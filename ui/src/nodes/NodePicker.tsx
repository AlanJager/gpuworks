import { forwardRef, useMemo, useState, type ReactNode } from "react";
import { Select, type RefSelectProps, type SelectProps } from "antd";
import { api, usePolling, type Node, type NodeFilter } from "../api";
import { nodeDisplayName } from "../nodeDisplay";
import { useNodeLookup } from "./useNodeLookup";

/** How many nodes one search offers; typing narrows the list on the server. */
export const NODE_PICKER_PAGE_SIZE = 50;

type Value = string | string[] | undefined;

type Props = {
  value?: Value;
  onChange?: (value: Value, nodes: Node[]) => void;
  mode?: "multiple";
  filter?: NodeFilter;
  optionLabel?: (node: Node) => ReactNode;
  optionDisabled?: (node: Node) => boolean;
  optionTitle?: (node: Node) => string;
  unknownLabel?: (uuid: string) => string;
  placeholder?: string;
  disabled?: boolean;
  allowClear?: boolean;
  style?: React.CSSProperties;
  notFoundContent?: ReactNode;
  maxCount?: number;
  size?: "small" | "middle" | "large";
  "aria-label"?: string;
  id?: string;
} & Pick<SelectProps, "aria-invalid" | "aria-describedby" | "aria-required">;

/**
 * NodePicker selects nodes by searching them on the server (GET /nodes with
 * page parameters) instead of listing the fleet: it offers one page of the
 * nodes `filter` matches, narrowed by what the user types. Chosen nodes keep
 * their names when a later search leaves them out, and a chosen uuid that no
 * longer names a node is still shown (unknownLabel) so it can be removed.
 *
 * It works as an antd Form control (value/onChange).
 */
const NodePicker = forwardRef<RefSelectProps, Props>(function NodePicker({
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
  const [search, setSearch] = useState("");
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
    const out: Array<{ value: string; label: ReactNode; disabled?: boolean; title?: string }> = [];
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
  return (
    <Select
      ref={ref}
      id={id}
      mode={mode}
      showSearch
      filterOption={false}
      onSearch={setSearch}
      onOpenChange={(open) => { if (!open) setSearch(""); }}
      value={value}
      onChange={(next: Value) => {
        const uuids = next === undefined ? [] : Array.isArray(next) ? next : [next];
        onChange?.(next, uuids.map((uuid) => known.get(uuid) ?? items?.find((node) => node.uuid === uuid)).filter((node): node is Node => !!node));
      }}
      options={options}
      loading={page.loading}
      placeholder={placeholder}
      disabled={disabled}
      allowClear={allowClear}
      style={style}
      notFoundContent={notFoundContent}
      maxCount={maxCount}
      size={size}
      aria-label={ariaLabel}
      aria-invalid={ariaInvalid}
      aria-describedby={ariaDescribedBy}
      aria-required={ariaRequired}
    />
  );
});

export default NodePicker;
