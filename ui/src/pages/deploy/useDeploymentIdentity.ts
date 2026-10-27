import { useEffect, useRef } from "react";
import type { FormInstance } from "antd";
import type { Model } from "../../api";

const SUFFIX_ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789";

// servedNameSuffix is the random "-abcd" a proposed served name ends in. The
// server refuses a served name another deployment holds, and a model is often
// deployed more than once, so the bare model name is never proposed.
export function servedNameSuffix(random: () => number = Math.random): string {
  let suffix = "";
  for (let i = 0; i < 4; i++) suffix += SUFFIX_ALPHABET[Math.floor(random() * SUFFIX_ALPHABET.length)];
  return `-${suffix}`;
}

// useDeploymentIdentity proposes the name and served name of a new
// deployment from its model. A form opened from a draft already has its names
// (an edit cannot change them), so `enabled` is false there.
export default function useDeploymentIdentity(
  form: FormInstance,
  model?: Model,
  enabled = true,
) {
  const generatedName = useRef("");
  const generatedServedName = useRef("");
  // One suffix per form: picking another model keeps it, so the proposal
  // changes only where the model name does.
  const suffix = useRef("");

  useEffect(() => {
    if (!model || !enabled) return;
    const nextName =
      (model.name || model.source_id)
        .split("/")
        .pop()
        ?.toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-|-$/g, "") || `model-${model.id}`;
    if (!suffix.current) suffix.current = servedNameSuffix();
    const nextServedName = (model.name || model.source_id) + suffix.current;
    const currentName = String(form.getFieldValue("name") || "");
    const currentServedName = String(form.getFieldValue("served_name") || "");
    const updates: Record<string, unknown> = {};
    if (!currentName || currentName === generatedName.current) {
      updates.name = nextName;
      generatedName.current = nextName;
    }
    if (
      (!currentServedName && !form.isFieldTouched("served_name")) ||
      (generatedServedName.current &&
        currentServedName === generatedServedName.current)
    ) {
      updates.served_name = nextServedName;
      generatedServedName.current = nextServedName;
    }
    if (Object.keys(updates).length) form.setFieldsValue(updates);
  }, [form, model, enabled]);
}
