import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { DetectedTemplateChip, InferenceTemplateChip } from "../../api";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../../components/ui/dialog";
import { Button } from "../../components/ui/button";
import { Checkbox } from "../../components/ui/checkbox";
import { Input } from "../../components/ui/input";
import VendorLogo from "../../components/VendorLogo";
import { compareVendors } from "../../vendors";
import { chipDisplayLabel, chipKey } from "../inferenceTemplateFlat";

export function normalizeCatalogChip(chip: InferenceTemplateChip): DetectedTemplateChip {
  return { vendor: chip.vendor, name: chip.name, cc: chip.cc == null ? undefined : String(chip.cc), arch_id: chip.arch_id };
}

export function TemplateChipPicker({ catalog, existing, onClose, onAdd }: {
  catalog: InferenceTemplateChip[]; existing: DetectedTemplateChip[]; onClose: () => void; onAdd: (chips: DetectedTemplateChip[]) => void;
}) {
  const { t } = useTranslation("inferenceTemplates");
  const chips = [...new Map(catalog.map(chip => { const normalized = normalizeCatalogChip(chip); return [chipKey(normalized), normalized]; })).values()];
  const vendors = [...new Set(chips.map(chip => chip.vendor))].sort(compareVendors);
  const [vendor, setVendor] = useState(vendors[0] || "");
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const existingKeys = new Set(existing.map(chipKey));
  const pending = chips.filter(chip => selected.has(chipKey(chip)) && !existingKeys.has(chipKey(chip)));
  const needle = query.trim().toLowerCase();
  const shown = chips.filter(chip => needle ? `${chipDisplayLabel(chip)} ${t(`vendor.${chip.vendor}`, { defaultValue: chip.vendor })}`.toLowerCase().includes(needle) : chip.vendor === vendor);
  return <Dialog open onOpenChange={open => { if (!open) onClose(); }}>
    <DialogContent size="lg" className="w-[min(720px,calc(100vw-32px))] max-w-none">
      <DialogHeader><DialogTitle>{t("flat.addHypothetical")}</DialogTitle></DialogHeader>
      <Input aria-label={t("chips.search")} placeholder={t("chips.search")} value={query} onChange={event => setQuery(event.target.value)} />
      <div className="template-chip-picker">
        <nav aria-label={t("columns.vendor")}>{vendors.map(item => <Button key={item} variant="ghost" active={item === vendor} onClick={() => { setVendor(item); setQuery(""); }} aria-label={t(`vendor.${item}`, { defaultValue: item })}><VendorLogo vendor={item} size={18} /></Button>)}</nav>
        <div className="template-chip-options">{shown.map(chip => { const key = chipKey(chip); const locked = existingKeys.has(key); return <label key={key}>
          <Checkbox checked={locked || selected.has(key)} disabled={locked} onCheckedChange={checked => setSelected(current => { const next = new Set(current); if (checked) next.add(key); else next.delete(key); return next; })} />
          <span>{chipDisplayLabel(chip)}</span>{locked && <small>{t("chips.alreadyAdded")}</small>}
        </label>; })}{!shown.length && <p className="text-fg-muted">{t("flat.empty")}</p>}</div>
      </div>
      <div className="template-chip-selection">{pending.map(chip => <Button key={chipKey(chip)} size="sm" variant="tertiary" onClick={() => setSelected(current => new Set([...current].filter(key => key !== chipKey(chip))))}>{chipDisplayLabel(chip)} ×</Button>)}</div>
      <DialogFooter><Button variant="ghost" onClick={onClose}>{t("flat.cancel")}</Button><Button disabled={!pending.length} onClick={() => onAdd(pending)}>{t("chips.addSelected", { count: pending.length })}</Button></DialogFooter>
    </DialogContent>
  </Dialog>;
}
