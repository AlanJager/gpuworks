import { useState } from "react";
import { message } from "antd";
import type { FormInstance } from "antd";
import { useTranslation } from "react-i18next";
import {
  api,
  type DeploymentDraft,
  type DeploymentEditApply,
  type DeploymentEditPlan,
} from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import { changedFormValues } from "./deployEditPayload";

/** What the confirm dialog needs: the plan to show, or the error that stopped it. */
export interface EditReview {
  plan?: DeploymentEditPlan;
  error?: unknown;
  body: Record<string, unknown>;
}

/** The deployer's choice in the dialog; apply is left out when the edit restarts nothing. */
export interface EditChoice {
  apply?: DeploymentEditApply;
  start?: boolean;
  wake?: boolean;
}

// useDeployEditSubmit is the edit's save: validate the form, ask the server
// what the edit would do (PUT ?dry_run=1), hand the plan to the confirm
// dialog, and write it with the way the deployer chose. A draft that moved
// since it was read (409) is read again under the deployer's own changes.
export default function useDeployEditSubmit({
  form,
  deploymentID,
  draft,
  buildBody,
  baselineValues,
  reload,
  onSaved,
}: {
  form: FormInstance;
  deploymentID: number;
  draft: DeploymentDraft;
  // The body of the edit the form holds now (null while the draft is not settled).
  buildBody: () => Record<string, unknown> | null;
  // The form values a draft reads as; the baseline an edit is measured from.
  baselineValues: (draft: DeploymentDraft) => Record<string, unknown>;
  reload: () => Promise<DeploymentDraft>;
  onSaved: () => void;
}) {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const [review, setReview] = useState<EditReview | null>(null);
  const [reviewing, setReviewing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [reloading, setReloading] = useState(false);

  const open = async () => {
    try {
      await form.validateFields();
    } catch (error) {
      // The failures are already beside their fields.
      if (error && typeof error === "object" && "errorFields" in error) return;
      throw error;
    }
    const body = buildBody();
    if (!body) return;
    setReviewing(true);
    try {
      const plan = await api.editDeployment(
        deploymentID,
        { deployment: body, base_revision: draft.revision },
        { dryRun: true },
      );
      setReview({ plan, body });
    } catch (error) {
      if (error instanceof APIError && error.code === "DEPLOYMENT_CHANGED") {
        setReview({ error, body });
      } else {
        message.error(t("edit.reviewFailed", { detail: apiErrorText(tCommon, error) }));
      }
    } finally {
      setReviewing(false);
    }
  };

  const save = async (choice: EditChoice) => {
    if (!review) return;
    setSaving(true);
    try {
      await api.editDeployment(deploymentID, {
        deployment: review.body,
        base_revision: draft.revision,
        ...(choice.apply ? { apply: choice.apply } : {}),
        ...(choice.start ? { start: true } : {}),
        ...(choice.wake ? { wake: true } : {}),
      });
      message.success(t("edit.saved"));
      setReview(null);
      onSaved();
    } catch (error) {
      // Kept inside the dialog, beside the choice that was refused.
      setReview({ ...review, error });
    } finally {
      setSaving(false);
    }
  };

  // reloadDraft reads the deployment's draft again and lays the deployer's own
  // edits - the form fields that differ from the draft they started from -
  // over it, so a change someone else made is picked up without losing theirs.
  const reloadDraft = async () => {
    const mine = changedFormValues(form.getFieldsValue(true), baselineValues(draft));
    setReloading(true);
    try {
      const fresh = await reload();
      const next = baselineValues(fresh);
      const gone = Object.fromEntries(
        Object.keys(baselineValues(draft))
          .filter((key) => !(key in next))
          .map((key) => [key, undefined]),
      );
      form.setFieldsValue({ ...gone, ...next, ...mine });
      setReview(null);
    } catch (error) {
      setReview((current) => (current ? { ...current, error } : current));
    } finally {
      setReloading(false);
    }
  };

  return {
    open,
    reviewing,
    review,
    saving,
    reloading,
    close: () => setReview(null),
    save,
    reloadDraft,
  };
}
