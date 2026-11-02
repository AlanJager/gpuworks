import { useEffect, useMemo, useState } from "react";
import type { FormInstance } from "antd";
import type { EngineProfile } from "../../api";
import { draftToFormValues } from "./deployDraftValues";

// useDeployDraftSetup finishes the form a draft opened. The draft alone
// cannot say which dictionary keys the deployment leaves unstated - that is
// the engine profile's list - so once the profile and the model's
// modalities are in, each published key the deployment does not state is set
// to "" (cleared, so the server adds no default). Until then the page is not
// ready and nothing is evaluated: an evaluation of the half-filled form would
// state the defaults. Answers true when the form is complete.
export default function useDeployDraftSetup({
  form,
  request,
  revision,
  profile,
  modalities,
  settled,
}: {
  form: FormInstance;
  request: Record<string, unknown>;
  revision: string;
  profile?: EngineProfile;
  modalities: string[];
  // The profile and the model have been asked for (answered or failed).
  settled: boolean;
}) {
  const [filledFor, setFilledFor] = useState("");
  const modalityKey = modalities.join(",");
  const stamp = `${revision}|${profile?.name ?? ""}|${modalityKey}`;
  useEffect(() => {
    if (!settled) return;
    const baseline = draftToFormValues(request, profile, modalities);
    const quick = (request.quick_params ?? {}) as Record<string, unknown>;
    const current = form.getFieldsValue(true) as Record<string, unknown>;
    const blanks: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(baseline))
      if (value === "" && !(key in quick) && current[key] === undefined) blanks[key] = "";
    if (Object.keys(blanks).length) form.setFieldsValue(blanks);
    setFilledFor(stamp);
  }, [form, settled, stamp]);
  return useMemo(() => settled && filledFor === stamp, [settled, filledFor, stamp]);
}
