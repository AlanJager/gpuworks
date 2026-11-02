import { useEffect, useMemo, useRef } from "react";
import type { FormInstance } from "antd";
import type { Deployment, DeploymentDraft, EngineProfile, Model, Node } from "../../api";
import type { DeployEdit } from "./DeployEditContext";
import { deployDraftPayload } from "./deployHelpers";
import { draftToFormValues } from "./deployDraftValues";
import { lockedControls } from "./deployEditLocks";
import { editPayload, payloadChanges } from "./deployEditPayload";
import useDeployEditSubmit from "./useDeployEditSubmit";

/** The deployment a deploy link opened: how, its draft, and how to read the draft again. */
export interface OpenedDeployment {
  mode: "edit" | "from";
  id: number;
  draft: DeploymentDraft;
  deployment?: Deployment;
  reload: () => Promise<DeploymentDraft>;
}

const NO_REQUEST: Record<string, unknown> = {};

// scrollToFields brings the controls a link names into view, once: antd gives
// a control the id of its field name. A control that is not on screen (folded,
// or in the other view) is skipped.
function useFocusFields(fields: string[], ready: boolean) {
  const done = useRef(false);
  const key = fields.join(",");
  useEffect(() => {
    if (!ready || done.current || !key) return;
    done.current = true;
    for (const field of key.split(",")) {
      const element = document.getElementById(field);
      if (!element) continue;
      element.scrollIntoView?.({ block: "center" });
      const holder = element.closest(".ant-form-item") ?? element;
      holder.classList.add("deploy-edit-highlight");
      setTimeout(() => holder.classList.remove("deploy-edit-highlight"), 2600);
    }
  }, [key, ready]);
}

// useDeployEditMode is everything the deploy page does differently when it
// was opened from a deployment's draft: edit it (the draft's locked controls
// stay as they are, the body goes to PUT) or clone it (the same fill, nothing
// locked, the ordinary create). Without `opened` it is inert.
export default function useDeployEditMode({
  form,
  opened,
  formValues,
  engineProfile,
  selectedModel,
  nodes,
  ready,
  focus,
  onSaved,
}: {
  form: FormInstance;
  opened?: OpenedDeployment;
  formValues: Record<string, unknown>;
  engineProfile?: EngineProfile;
  selectedModel?: Model;
  nodes: Node[];
  // The form is complete (see useDeployDraftSetup).
  ready: boolean;
  focus: string[];
  onSaved: () => void;
}) {
  const editing = opened?.mode === "edit";
  const draft = opened?.draft;
  const request = draft?.deployment ?? NO_REQUEST;
  const modalities = useMemo(
    () => selectedModel?.input_modalities ?? [],
    [selectedModel?.input_modalities],
  );
  useFocusFields(focus, ready && editing);

  const base = useMemo(
    () => ({ request, locked: editing ? (draft?.locked ?? []) : [] }),
    [request, editing, draft?.locked],
  );
  const context = useMemo<DeployEdit>(
    () => ({
      mode: opened?.mode ?? "create",
      deploymentID: opened?.id,
      locked: editing ? lockedControls(draft?.locked) : new Set<string>(),
    }),
    [opened?.mode, opened?.id, editing, draft?.locked],
  );
  const baselineValues = (source: DeploymentDraft) =>
    draftToFormValues(source.deployment, engineProfile, modalities);

  // The body the evaluation (and an edit) sends, for the values given.
  const bodyOf = (values: Record<string, unknown>) =>
    editing
      ? editPayload(values, nodes, engineProfile, modalities, base)
      : deployDraftPayload(values, nodes, engineProfile, modalities);

  const changes = useMemo(
    () =>
      editing && ready && draft
        ? payloadChanges(bodyOf(baselineValues(draft)), bodyOf(formValues))
        : [],
    // bodyOf and baselineValues are rebuilt from the values listed.
    [editing, ready, draft, formValues, nodes, engineProfile, modalities],
  );

  const submit = useDeployEditSubmit({
    form,
    deploymentID: opened?.id ?? 0,
    draft: draft ?? { deployment: NO_REQUEST, revision: "", locked: [] },
    buildBody: () => bodyOf(form.getFieldsValue(true)),
    baselineValues,
    reload: opened?.reload ?? (() => Promise.reject(new Error("no deployment to read"))),
    onSaved,
  });

  return { context, editing, changes, submit, bodyOf };
}
