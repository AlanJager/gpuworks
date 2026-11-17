import { useEffect, useRef } from "react";
import type { InferenceSpec } from "../../api";
import { defaultPdRoles, pdRolesFromShape } from "./deployPd";

// SpecDefaultsForm is the part of the deploy form the hook reads and writes;
// the page's form instance satisfies it.
type SpecDefaultsForm = {
  getFieldValue: (name: string) => unknown;
  setFieldsValue: (values: Record<string, unknown>) => void;
};

type SpecDefaults = Pick<
  InferenceSpec,
  "id" | "source" | "name" | "pd_mode" | "pd_roles" | "manual_command"
>;

// useDeploySpecDefaults lays what a template spec declares about its own
// launch onto a fresh create form, once per chosen spec: a PD spec switches
// the topology to PD with its mode (and, when it states a layout, the roles'
// TP/PP and the instance rows with empty follower slots for the deployer to
// fill), and a spec with its own launch script starts the deployment in
// manual command mode with that script. The numbers and the script are the
// server's, used as given. A form opened from a deployment (edit, clone)
// already holds its own values and is left alone, and a command the deployer
// typed first is never replaced.
export default function useDeploySpecDefaults({
  form,
  templateID,
  spec,
  enabled,
}: {
  form: SpecDefaultsForm;
  templateID?: number;
  spec?: SpecDefaults;
  enabled: boolean;
}) {
  const appliedKey = useRef("");
  const appliedScript = useRef("");
  useEffect(() => {
    if (!enabled || !spec || !templateID) return;
    const key = `${templateID}:${spec.id}:${spec.source}:${spec.name}`;
    if (appliedKey.current === key) return;
    appliedKey.current = key;

    const values: Record<string, unknown> = {};
    if (spec.pd_mode) {
      const grouped = ["multi", "pd"].includes(
        String(form.getFieldValue("node_selection_mode") || "auto"),
      );
      Object.assign(values, {
        node_selection_mode: "pd",
        nnodes: 1,
        replicas: 1,
        node_uuid: undefined,
        distributed_nodes: undefined,
        pd_mode: spec.pd_mode,
        // A layout with multi-node instances names its nodes by hand: the
        // platform's automatic placement takes one node per unit only.
        units_placement: spec.pd_roles
          ? "manual"
          : (form.getFieldValue("units_placement") ?? "auto"),
        pd_roles: spec.pd_roles
          ? pdRolesFromShape(spec.pd_roles)
          : form.getFieldValue("pd_roles") || defaultPdRoles(),
        // Grouped placements pack by default; a strategy the deployer chose stays.
        ...(grouped
          ? {}
          : {
              placement_strategy:
                form.getFieldValue("placement_strategy_chosen") &&
                form.getFieldValue("placement_strategy")
                  ? form.getFieldValue("placement_strategy")
                  : "binpack",
            }),
      });
    }

    // The script of the spec chosen before leaves with it, if untouched.
    const current = String(form.getFieldValue("container_cmd") || "");
    const previous = appliedScript.current;
    const untouchedPrevious = previous !== "" && current === previous;
    if (untouchedPrevious) {
      Object.assign(values, {
        command_mode: "engine",
        command_edit_previous_mode: undefined,
        container_cmd: undefined,
      });
      appliedScript.current = "";
    }
    if (spec.manual_command && (untouchedPrevious || current.trim() === "")) {
      Object.assign(values, {
        command_edit_previous_mode: "engine",
        command_mode: "manual",
        container_cmd: spec.manual_command,
      });
      appliedScript.current = spec.manual_command;
    }
    if (Object.keys(values).length) form.setFieldsValue(values);
  }, [enabled, form, spec, templateID]);
}
