import { useEffect, useRef } from "react";
import { Form, type FormInstance } from "antd";
import { api } from "../../api";

// useDeployPrefill performs the deploy form's one-time fills: the URL's query
// scheme (model, template, spec), the automatic-mode clearing, and the server
// defaults that land after the first evaluation. What a template contributes
// to the launch configuration is NOT prefilled - evaluate's form_values is
// that answer.
export default function useDeployPrefill({
  form,
  queryModelID,
  queryTemplateID,
  querySpecName,
  querySpecSource,
  selectedTemplate,
  selectedTemplateSpec,
  nodeSelectionMode,
  defaultsEngine,
}: {
  form: FormInstance;
  queryModelID?: number;
  queryTemplateID?: number;
  querySpecName?: string;
  querySpecSource?: string;
  selectedTemplate?: { id: number };
  selectedTemplateSpec?: {
    spec: {
      engine: string;
      gpus: { vendor: string };
      scenario?: string;
      image?: string;
    };
  };
  nodeSelectionMode: string;
  defaultsEngine?: string;
}) {
  const schemeRef = useRef("");
  // The URL's model is filled only once it is known to exist.
  useEffect(() => {
    if (!queryModelID) return;
    let alive = true;
    api.model(queryModelID).then(
      (model) => {
        if (alive && !form.getFieldValue("model_id")) {
          form.setFieldValue("model_id", model.id);
        }
      },
      () => {},
    );
    return () => {
      alive = false;
    };
  }, [form, queryModelID]);
  useEffect(() => {
    if (queryTemplateID && !form.getFieldValue("template_id")) {
      form.setFieldsValue({
        template_id: queryTemplateID,
        template_spec_source: querySpecSource,
        template_spec_name: querySpecName,
      });
    }
    if (nodeSelectionMode === "auto") {
      form.setFieldsValue({
        node_uuid: undefined,
        gpu_indexes: undefined,
        tp: undefined,
      });
    }
    if (
      !queryTemplateID ||
      !querySpecName ||
      selectedTemplate?.id !== queryTemplateID ||
      !selectedTemplateSpec
    ) {
      return;
    }
    const key = `${queryTemplateID}:${querySpecSource || ""}:${querySpecName}`;
    if (schemeRef.current === key) return;
    const spec = selectedTemplateSpec.spec;
    form.setFieldsValue({
      engine: spec.engine,
      target_vendor: spec.gpus.vendor,
      scenario: spec.scenario || undefined,
      image: spec.image || undefined,
    });
    schemeRef.current = key;
  }, [
    form,
    nodeSelectionMode,
    queryModelID,
    querySpecName,
    querySpecSource,
    queryTemplateID,
    selectedTemplate,
    selectedTemplateSpec,
  ]);

  // The engine default lands on the form so the dictionary renders from it,
  // and lands again whenever the engine is cleared (a dropped template takes
  // its engine with it). The watch only re-runs the check: on the first
  // render it reads undefined before the form's own values.
  const engine = Form.useWatch("engine", { form, preserve: true }) as
    | string
    | undefined;
  useEffect(() => {
    if (defaultsEngine && !form.getFieldValue("engine")) {
      form.setFieldValue("engine", defaultsEngine);
    }
  }, [defaultsEngine, engine, form]);
}
