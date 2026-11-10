import { CloseOutlined, HolderOutlined, LockOutlined } from "@ant-design/icons";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { DndContext, KeyboardSensor, PointerSensor, closestCenter, useSensor, useSensors, type DragEndEvent } from "@dnd-kit/core";
import { SortableContext, arrayMove, sortableKeyboardCoordinates, useSortable, verticalListSortingStrategy } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { useTranslation } from "react-i18next";
import type { ColumnPrefs } from "./columnPrefs";

export interface SettingsColumn {
  key: string;
  label: string;
  role?: "name" | "actions";
}

function SelectedItem({ column, onRemove }: { column: SettingsColumn; onRemove: () => void }) {
  const { t } = useTranslation("common");
  const sortable = useSortable({ id: column.key });
  return (
    <div
      ref={sortable.setNodeRef}
      className="resource-table-settings-item"
      style={{ transform: CSS.Transform.toString(sortable.transform), transition: sortable.transition }}
    >
      <span className="resource-table-settings-handle" {...sortable.attributes} {...sortable.listeners} aria-label={t("resourceTable.dragColumn", { name: column.label })}>
        <HolderOutlined />
      </span>
      <span className="resource-table-settings-label">{column.label}</span>
      <Button variant="ghost" size="sm" iconOnly onClick={onRemove} aria-label={t("resourceTable.hideColumn", { name: column.label })}><CloseOutlined /></Button>
    </div>
  );
}

function LockedItem({ column }: { column: SettingsColumn }) {
  return (
    <div className="resource-table-settings-item resource-table-settings-locked">
      <span className="resource-table-settings-handle"><LockOutlined /></span>
      <span className="resource-table-settings-label">{column.label}</span>
    </div>
  );
}

/**
 * The column settings drawer: the shown columns on the left, in order and
 * draggable; every column on the right, as checkboxes. The name and actions
 * columns are always shown and never move.
 */
export default function ColumnSettingsDrawer({ open, columns, prefs, onChange, onReset, onClose }: {
  open: boolean;
  columns: SettingsColumn[];
  prefs: ColumnPrefs;
  onChange: (next: ColumnPrefs) => void;
  onReset: () => void;
  onClose: () => void;
}) {
  const { t } = useTranslation("common");
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );
  const byKey = new Map(columns.map((column) => [column.key, column]));
  const hidden = new Set(prefs.hidden);
  const shown = prefs.order.filter((key) => !hidden.has(key));
  const leading = columns.filter((column) => column.role === "name");
  const trailing = columns.filter((column) => column.role === "actions");
  const setHidden = (key: string, hide: boolean) =>
    onChange({ ...prefs, hidden: hide ? [...prefs.hidden, key] : prefs.hidden.filter((item) => item !== key) });
  const dragEnd = ({ active, over }: DragEndEvent) => {
    if (!over || active.id === over.id) return;
    const from = prefs.order.indexOf(String(active.id));
    const to = prefs.order.indexOf(String(over.id));
    if (from < 0 || to < 0) return;
    onChange({ ...prefs, order: arrayMove(prefs.order, from, to) });
  };
  const total = columns.length;
  const shownCount = leading.length + shown.length + trailing.length;
  return (
    <SideDrawer
      open={open}
      onClose={onClose}
      width={560}
      title={t("resourceTable.settingsTitle")}
      className="resource-table-settings"
      footer={<Button onClick={onReset}>{t("resourceTable.reset")}</Button>}
    >
      <div className="resource-table-settings-panes">
        <section aria-label={t("resourceTable.selected", { count: shownCount })}>
          <strong className="resource-table-settings-heading">{t("resourceTable.selected", { count: shownCount })}</strong>
          <div className="resource-table-settings-selected">
            {leading.map((column) => <LockedItem key={column.key} column={column} />)}
            <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={dragEnd}>
              <SortableContext items={shown} strategy={verticalListSortingStrategy}>
                {shown.map((key) => byKey.get(key)).filter((column): column is SettingsColumn => !!column).map((column) => (
                  <SelectedItem key={column.key} column={column} onRemove={() => setHidden(column.key, true)} />
                ))}
              </SortableContext>
            </DndContext>
            {trailing.map((column) => <LockedItem key={column.key} column={column} />)}
          </div>
        </section>
        <section aria-label={t("resourceTable.all", { shown: shownCount, total })}>
          <strong className="resource-table-settings-heading">{t("resourceTable.all", { shown: shownCount, total })}</strong>
          <div className="resource-table-settings-all">
            {leading.map((column) => <label key={column.key} className="flex items-center gap-2"><Checkbox checked disabled />{column.label}</label>)}
            {prefs.order.map((key) => byKey.get(key)).filter((column): column is SettingsColumn => !!column).map((column) => (
              <label key={column.key} className="flex items-center gap-2"><Checkbox checked={!hidden.has(column.key)} onCheckedChange={(checked) => setHidden(column.key, !checked)} />{column.label}</label>
            ))}
            {trailing.map((column) => <label key={column.key} className="flex items-center gap-2"><Checkbox checked disabled />{column.label}</label>)}
          </div>
        </section>
      </div>
    </SideDrawer>
  );
}
