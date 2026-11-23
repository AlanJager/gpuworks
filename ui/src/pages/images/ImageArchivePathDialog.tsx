import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "../../components/ui/alert";
import { Button } from "../../components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../components/ui/dialog";
import { HelpTip } from "../../ui-kit/HelpTip";
import { Input } from "../../components/ui/input";

import { isAbsoluteArchivePath } from "./ImagePageHelpers";
import { apiErrorText } from "../../apiError";

type Props = {
  replace: boolean;
  imageRef: string;
  nodeLabel: string;
  onCancel: () => void;
  onStart: (path: string) => Promise<void>;
};

export default function ImageArchivePathDialog({ imageRef, replace, nodeLabel, onCancel, onStart }: Props) {
  const { t } = useTranslation("images");
  const { t: common } = useTranslation("common");
  const [path, setPath] = useState("");
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async () => {
    if (!isAbsoluteArchivePath(path) || starting) return;
    setStarting(true);
    setError(null);
    try {
      await onStart(path.trim());
    } catch (e) {
      setError(apiErrorText(common, e) || t("action.pullFailed"));
    } finally {
      setStarting(false);
    }
  };
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !starting) onCancel(); }}>
      <DialogContent size="lg" closeButtonLabel={common("actions.close")} closeButtonDisabled={starting}>
        <DialogHeader>
          <DialogTitle>{t("nodeTable.localDialog.title", { node: nodeLabel })}</DialogTitle>
          <DialogDescription>{t(replace ? "nodeTable.localDialog.replaceDescription" : "nodeTable.localDialog.description", { ref: imageRef })}</DialogDescription>
        </DialogHeader>
        <form onSubmit={(event) => { event.preventDefault(); void submit(); }}>
          <div className="space-y-2">
            <div className="flex items-center gap-1">
              <label htmlFor="image-archive-path" className="text-body">{t("nodeTable.localDialog.path")}</label>
              <HelpTip content={t("nodeTable.localDialog.help")} />
            </div>
            <Input
              id="image-archive-path"
              value={path}
              disabled={starting}
              placeholder="/data/images/runner.tar"
              onChange={(event) => { setPath(event.target.value); setError(null); }}
            />
            {path.trim() && !isAbsoluteArchivePath(path) && <p role="alert">{t("import.local.pathRequired")}</p>}
            {error ? <Alert status="danger"><AlertDescription>{error}</AlertDescription></Alert> : null}
          </div>
          <DialogFooter>
            <Button type="button" disabled={starting} onClick={onCancel}>{t("common.cancel")}</Button>
            <Button type="submit" variant="primary" disabled={starting || !isAbsoluteArchivePath(path)} aria-busy={starting}>
              {t(replace ? "nodeTable.localDialog.replace" : "nodeTable.localDialog.start")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
