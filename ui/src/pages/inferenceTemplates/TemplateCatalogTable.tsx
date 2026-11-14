import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { InferenceTemplate } from "../../api";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import VendorLogo from "../../components/VendorLogo";
import { Badge } from "../../components/ui/badge";
import { compactModelMetadata } from "../inferenceTemplateFlat";
import { inputModalitiesOf, inputModalityLabelKey } from "../../modelInference";
import { compareVendors } from "../../vendors";

export function templateDetailPath(modelID: string, catalogQuery: string) {
  const search = new URLSearchParams();
  if (catalogQuery) search.set("from", catalogQuery);
  return `/inference-templates/${modelID}${search.size ? `?${search}` : ""}`;
}

export function TemplateCatalogTable({ models, catalogQuery, onRefresh }: {
  models: InferenceTemplate[]; catalogQuery: string; onRefresh: () => void;
}) {
  const { t } = useTranslation("inferenceTemplates");
  const vendors = [...new Set(models.flatMap(model => model.specs.map(spec => spec.gpus.vendor)))].sort(compareVendors);
  const quantizations = [...new Set(models.flatMap(model => model.specs.map(spec => spec.quantization).filter((value): value is string => Boolean(value))))];
  const columns: ResourceColumn<InferenceTemplate>[] = [
    { key: "model", role: "name", title: t("columns.model"), width: 260, render: (_, model) => <>
      <Link className="text-fg-brand font-normal" to={templateDetailPath(model.model_id, catalogQuery)} title={model.display_title || model.model_id}>{model.display_title || model.model_id.split("/").pop()}</Link>
      <span className="resource-table-sub" title={model.model_id}>{compactModelMetadata(model) || model.model_id}</span>
    </> },
    { key: "vendor", title: t("columns.vendor"), width: 230,
      filters: vendors.map(value => ({ value, text: t(`vendor.${value}`, { defaultValue: value }) })),
      onFilter: (value, model) => model.specs.some(spec => spec.gpus.vendor === value),
      render: (_, model) => <div className="template-table-vendors">{[...new Set(model.specs.map(spec => spec.gpus.vendor))].sort(compareVendors).map(vendor => <VendorLogo key={vendor} vendor={vendor} size={18} />)}</div> },
    { key: "input", title: t("columns.inputModalities"), width: 150, render: (_, model) => <div className="template-table-tags">{inputModalitiesOf(model.input_modalities).map(value => <Badge key={value} size="sm">{t(inputModalityLabelKey[value])}</Badge>)}</div> },
    { key: "weights", title: t("columns.weightVariants"), width: 300,
      filters: quantizations.map(value => ({ value, text: value.toUpperCase() })),
      onFilter: (value, model) => model.specs.some(spec => spec.quantization === value),
      render: (_, model) => <div className="template-table-tags">{[...new Set(model.specs.map(spec => `${(spec.quantization || "unknown").toUpperCase()} · ${spec.weight_sources[0]?.repo_id || spec.name}`))].map(label => <Badge key={label} size="sm" title={label} className="max-w-full truncate">{label}</Badge>)}</div> },
    { key: "engine", title: t("columns.engine"), width: 150, render: (_, model) => <div className="template-table-tags">{[...new Set(model.specs.map(spec => spec.engine))].map(engine => <Badge key={engine} size="sm">{engine}</Badge>)}</div> },
  ];
  return <ResourceTable tableId="inference-template-catalog" rowKey="model_id" columns={columns} data={models}
    onRefresh={onRefresh} pagination={false} emptyDescription={t("flat.empty")} filteredEmptyDescription={t("flat.empty")} />;
}
