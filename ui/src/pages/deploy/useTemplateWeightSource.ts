import { useEffect, useRef, useState } from "react";
import { message } from "antd";
import type { FormInstance } from "antd";
import { useTranslation } from "react-i18next";
import {
  api,
  type EngineProfile,
  type InferenceTemplate,
  type ModelCore,
  type ModelPresenceSummary,
  type WeightSource,
} from "../../api";
import { modelServesWeightSource } from "./deployHelpers";

/** A registered model as the lookup lists it; presence says where its files are. */
export type WeightModel = ModelCore & { presence?: ModelPresenceSummary };

/** How many registered models one weight source lookup reads. */
const LOOKUP_PAGE_SIZE = 50;
const LOOKUP_RETRY_MS = 15000;

/**
 * findWeightSourceModels reads the registered models whose source id is one
 * of the sources' repo ids (GET /models?source_id=, exact and ignoring case);
 * the subfolder and provider are judged here by the caller.
 */
export async function findWeightSourceModels(
  sources: WeightSource[],
): Promise<WeightModel[]> {
  const ids = [
    ...new Set(
      sources.map((source) => String(source.repo_id || "").trim()).filter(Boolean),
    ),
  ];
  if (!ids.length) return [];
  const page = await api.modelsPage({
    page: 1,
    page_size: LOOKUP_PAGE_SIZE,
    extra: { source_id: ids },
  });
  return page.items;
}

// useTemplateWeightSource owns the template's weight artifact choice: the
// spec's declared mirrors, the explicit source choice in the modal, and the
// one registration action through the existing register API. Its model
// matching is the server's repo-identity judgement.
export default function useTemplateWeightSource({
  form,
  template,
  selection,
  engineProfile,
}: {
  form: FormInstance;
  template?: InferenceTemplate;
  selection?: { spec: import("../../api").InferenceSpec };
  engineProfile?: EngineProfile;
}) {
  const { t } = useTranslation("deploy");
  const [modalOpen, setModalOpen] = useState(false);
  const [choice, setChoice] = useState<WeightSource>();
  const [registering, setRegistering] = useState(false);
  // The registered models serving the selected spec's sources, once the
  // lookup for that spec (key) has answered; a pending or failed lookup
  // leaves it on another key, and nothing is registered meanwhile.
  const [lookup, setLookup] = useState<{ key: string; models: WeightModel[] }>();
  // A failed lookup is retried at the pace the model list used to poll.
  const [lookupRetry, setLookupRetry] = useState(0);
  const stateRef = useRef({ applied: "", prompted: "", registering: "" });
  // What the user settled on for this spec: a registered model or a declared
  // source (whose model, registered now or already there, is then applied).
  const pickedRef = useRef<{ key: string; model?: WeightModel; source?: WeightSource }>();
  // The registered models matching a declared source, listed for the user to
  // confirm (5.13), and which one (or the template's own source) they picked.
  const [matches, setMatches] = useState<WeightModel[]>([]);
  const [useModel, setUseModel] = useState<number | "template">();
  // The model the form now carries for this template, to show its source.
  const [appliedModel, setAppliedModel] = useState<WeightModel>();
  const key =
    template && selection
      ? `${template.id}:${selection.spec.source}:${selection.spec.name}`
      : "";
  const sources: WeightSource[] = selection?.spec.weight_sources || [];

  useEffect(() => {
    if (!key) return;
    const provider = form.getFieldValue(["model_source", "provider"]);
    // A model-only default belongs to the previous scope. On a new spec,
    // retain a provider only when that spec declares it; storage is independent.
    if (
      typeof provider === "string" && provider.trim() &&
      !sources.some((source) => source.provider.toLowerCase() === provider.trim().toLowerCase())
    ) form.setFieldValue(["model_source", "provider"], undefined);
  }, [form, key]);


  useEffect(() => {
    if (!key || !sources.length || !sources[0]?.repo_id) return;
    let alive = true;
    let retry: ReturnType<typeof setTimeout> | undefined;
    findWeightSourceModels(sources).then(
      (models) => {
        if (alive) setLookup({ key, models });
      },
      (reason: Error) => {
        if (!alive) return;
        message.error(t("action.modelLookupFailed", { detail: reason.message }));
        retry = setTimeout(() => setLookupRetry((n) => n + 1), LOOKUP_RETRY_MS);
      },
    );
    return () => {
      alive = false;
      clearTimeout(retry);
    };
  }, [key, lookupRetry]);

  const register = async (source: WeightSource, registrationKey: string) => {
    if (!source.repo_id || stateRef.current.registering === registrationKey)
      return;
    stateRef.current.registering = registrationKey;
    setRegistering(true);
    try {
      const model = await api.quickRegisterModel({
        source: source.provider,
        source_id: source.repo_id,
        revision: source.revision,
        artifact_subfolder: source.subfolder,
        name: source.repo_id,
        weight_bytes: selection?.spec.weight_bytes,
        quantization: selection?.spec.quantization,
        architecture: template?.architecture,
        context_length: template?.context_length,
      });
      form.setFieldValue("model_id", model.id);
      pickedRef.current = { key: registrationKey, model };
      // The registered model now serves the spec: the template is applied
      // to the form as for a model that was already registered.
      setLookup((prev) =>
        prev?.key === registrationKey
          ? { key: registrationKey, models: [...prev.models, model] }
          : prev,
      );
      setModalOpen(false);
      setChoice(undefined);
      message.success(t("action.modelRegistered"));
      return model;
    } catch (reason) {
      stateRef.current.registering = "";
      message.error(
        t("action.modelRegisterFailed", { detail: (reason as Error).message }),
      );
      return undefined;
    } finally {
      setRegistering(false);
    }
  };

  const modelFor = (source: WeightSource, models: WeightModel[]) =>
    models.find(
      (item) =>
        item.source?.toLowerCase() === source.provider.toLowerCase() &&
        modelServesWeightSource(item, source),
    );

  useEffect(() => {
    if (!template || !selection || !key) return;
    if (stateRef.current.applied === key) return;
    if (!sources.length || !sources[0]?.repo_id) return;
    // Wait for the lookup before asking, choosing or registering: an answer
    // not yet in (or a failed one) must not create a duplicate model.
    if (lookup?.key !== key) return;
    const picked =
      pickedRef.current?.key === key ? pickedRef.current : undefined;
    if (!picked) {
      // A registered model whose repo id is one the template declares is
      // never taken silently: the user confirms it, or picks a source.
      const found = lookup.models.filter((item) =>
        sources.some((candidate) => modelServesWeightSource(item, candidate)),
      );
      // Several sources with nothing registered: the user chooses, with no
      // default. One source with nothing registered: nothing to ask.
      if (found.length || sources.length > 1) {
        if (stateRef.current.prompted === key) return;
        stateRef.current.prompted = key;
        setMatches(found);
        setAppliedModel(undefined);
        setUseModel(found.length === 1 ? found[0].id : undefined);
        setChoice(undefined);
        setModalOpen(true);
        return;
      }
    }
    // A chosen source is served only by its own provider's model.
    const model =
      picked?.model ??
      (picked?.source ? modelFor(picked.source, lookup.models) : undefined);
    if (model) {
      if (!engineProfile || engineProfile.name !== selection.spec.engine)
        return;
      form.setFieldsValue({
        model_id: model.id,
        template_id: template.id,
        template_spec_source: selection.spec.source,
        template_spec_name: selection.spec.name,
        scenario: selection.spec.scenario || undefined,
        engine: selection.spec.engine,
        image: selection.spec.image,
        image_mode: "custom",
        container_env_rows: Object.entries(selection.spec.env || {}).map(
          ([envKey, value]) => ({ key: envKey, value }),
        ),
      });
      setAppliedModel(model);
      stateRef.current.applied = key;
      return;
    }
    if (stateRef.current.registering === key) return;
    if (sources.length === 1 && !picked) {
      void register(sources[0], key);
    }
  }, [engineProfile, lookup, selection, template]);

  const confirm = async () => {
    if (!selection) return;
    const confirmKey = `${template?.id || 0}:${selection.spec.source}:${selection.spec.name}`;
    if (typeof useModel === "number") {
      // The user confirmed a registered model: it serves the template.
      const picked = matches.find((item) => item.id === useModel);
      if (!picked) return;
      pickedRef.current = { key: confirmKey, model: picked };
      form.setFieldValue("model_id", picked.id);
      setLookup((prev) => ({
        key: confirmKey,
        models: prev?.key === confirmKey ? prev.models : [picked],
      }));
      setModalOpen(false);
      setChoice(undefined);
      return;
    }
    if (!choice) return;
    pickedRef.current = { key: confirmKey, source: choice };
    // The choice is matched against a fresh lookup, never a missing one: a
    // failed lookup registers nothing.
    let models: WeightModel[];
    try {
      models = await findWeightSourceModels([choice]);
    } catch (reason) {
      message.error(t("action.modelLookupFailed", { detail: (reason as Error).message }));
      return;
    }
    const existing = modelFor(choice, models);
    if (existing) {
      form.setFieldValue("model_id", existing.id);
      // The chosen source's model applies the template, as a single
      // source's would.
      setLookup({ key: confirmKey, models });
      setModalOpen(false);
      setChoice(undefined);
      return;
    }
    void register(choice, confirmKey);
  };

  return {
    modalOpen,
    choice,
    setChoice,
    registering,
    setModalOpen,
    confirm,
    register,
    matches,
    useModel,
    setUseModel,
    appliedModel,
  };
}
