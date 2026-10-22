import { useMemo } from "react";
import { Form } from "antd";
import type { FormInstance } from "antd";
import type { InferenceTemplate, InferenceTemplateIndexItem } from "../../api";
import { useInferenceTemplateDetail } from "./useInferenceTemplateDetail";

// useDeployTemplateSelection resolves the template and spec the form points
// at: the watched template/spec fields, falling back to the URL's query scheme
// for a link that has not touched the form yet.
export default function useDeployTemplateSelection({
  form,
  templateIndex,
  queryTemplateID,
  querySpecName,
  querySpecSource,
}: {
  form: FormInstance;
  templateIndex: { items?: InferenceTemplateIndexItem[] } | undefined;
  queryTemplateID?: number;
  querySpecName?: string;
  querySpecSource?: string;
}) {
  const watchOptions = { form, preserve: true } as const;
  const watchedTemplateID = Form.useWatch("template_id", watchOptions) as
    number | undefined;
  const activeTemplateID = watchedTemplateID || queryTemplateID;
  const activeTemplateModelID = templateIndex?.items?.find(
    (item) => item.id === activeTemplateID,
  )?.model_id;
  const selectedTemplate = useInferenceTemplateDetail(activeTemplateModelID);
  const templates = useMemo(
    () => (selectedTemplate ? [selectedTemplate] : []),
    [selectedTemplate],
  );
  const watchedSpecName = Form.useWatch("template_spec_name", watchOptions) as
    string | undefined;
  const watchedSpecSource = Form.useWatch(
    "template_spec_source",
    watchOptions,
  ) as string | undefined;
  const selectedTemplateSpec = useMemo(() => {
    const name =
      watchedSpecName ||
      (activeTemplateID === queryTemplateID ? querySpecName : undefined);
    const source =
      watchedSpecSource ||
      (activeTemplateID === queryTemplateID ? querySpecSource : undefined);
    const spec = name
      ? (selectedTemplate as InferenceTemplate | undefined)?.specs.find(
          (candidate) =>
            candidate.name === name && (!source || candidate.source === source),
        )
      : undefined;
    return spec ? { spec } : undefined;
  }, [
    activeTemplateID,
    querySpecName,
    querySpecSource,
    queryTemplateID,
    selectedTemplate,
    watchedSpecName,
    watchedSpecSource,
  ]);
  return { selectedTemplate, templates, selectedTemplateSpec };
}
