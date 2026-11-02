import { useCallback, useEffect, useRef, useState } from "react";
import { api, type Deployment, type DeploymentDraft } from "../../api";

export type DeploymentDraftState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "error"; error: unknown }
  | { status: "ready"; id: number; draft: DeploymentDraft; deployment?: Deployment };

// useDeploymentDraft reads the draft of the deployment a deploy link opens
// (edit=<id> or from=<id>) once, and again on reload() - which answers the
// fresh draft so a caller can lay its own edits over it. The deployment read
// beside it only labels the page (name, state); the draft alone decides
// whether the page can open.
export default function useDeploymentDraft(id: number | undefined) {
  const [state, setState] = useState<DeploymentDraftState>(
    id ? { status: "loading" } : { status: "idle" },
  );
  const seq = useRef(0);

  const read = useCallback(async (target: number) => {
    const [draft, deployment] = await Promise.all([
      api.deploymentDraft(target),
      api.deployment(target).catch(() => undefined),
    ]);
    return { id: target, draft, deployment };
  }, []);

  useEffect(() => {
    const mine = ++seq.current;
    if (!id) {
      setState({ status: "idle" });
      return;
    }
    setState({ status: "loading" });
    read(id).then(
      (loaded) => {
        if (mine === seq.current) setState({ status: "ready", ...loaded });
      },
      (error: unknown) => {
        if (mine === seq.current) setState({ status: "error", error });
      },
    );
    return () => {
      seq.current++;
    };
  }, [id, read]);

  const reload = useCallback(async (): Promise<DeploymentDraft> => {
    if (!id) throw new Error("no deployment to read");
    const mine = ++seq.current;
    const loaded = await read(id);
    if (mine === seq.current) setState({ status: "ready", ...loaded });
    return loaded.draft;
  }, [id, read]);

  return { state, reload };
}
