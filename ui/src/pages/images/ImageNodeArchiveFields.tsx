import { useEffect, useRef, useState } from "react";
import type { UseFormReturn } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { api, type ArchiveContents, type ArchiveImageMetadata } from "../../api";
import { isAbsoluteArchivePath } from "./ImagePageHelpers";
import { apiErrorText } from "../../apiError";
import { Alert, AlertDescription } from "../../components/ui/alert";
import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import NodePicker from "../../nodes/NodePicker";
import { FormField } from "../../ui-kit/form/FormField";
import { HelpTip } from "../../ui-kit/HelpTip";

export type NodeArchive = { node: string; path: string };

type Props = {
  archive: UseFormReturn<NodeArchive>;
  busy: boolean;
  setBusy: (busy: boolean) => void;
  onPick: (ref: string, metadata?: ArchiveImageMetadata) => void;
  onInvalidate: () => void;
};

export default function ImageNodeArchiveFields({ archive, busy, setBusy, onPick, onInvalidate }: Props) {
  const { t } = useTranslation("images");
  const { t: common } = useTranslation("common");
  const pending = useRef<AbortController | null>(null);
  const [preview, setPreview] = useState<ArchiveContents | null>(null);
  const tags = preview?.repo_tags;
  const [picked, setPicked] = useState("");
  const [error, setError] = useState("");
  useEffect(() => () => { pending.current?.abort(); setBusy(false); }, [setBusy]);

  const invalidate = () => {
    pending.current?.abort();
    setBusy(false);
    setError("");
    setPreview(null);
    onInvalidate();
    setPicked("");
  };
  const pick = (ref: string, contents: ArchiveContents | null) => {
    onPick(ref, contents?.metadata_by_tag?.[ref]);
    setPicked(ref);
  };
  const resolve = async () => {
    invalidate();
    const request = new AbortController();
    pending.current = request;
    setBusy(true);
    try {
      if (!(await archive.trigger()) || request.signal.aborted) return;
      const { node, path } = archive.getValues();
      const result = await api.previewImageArchivePath(node, path.trim(), request.signal);
      if (request.signal.aborted) return;
      setPreview(result);
      if (result.repo_tags.length) pick(result.repo_tags[0], result);
    } catch (e) {
      if (!request.signal.aborted) setError(apiErrorText(common, e));
    } finally {
      if (!request.signal.aborted) setBusy(false);
    }
  };

  return (
    <div className="grid gap-3">
      <FormField control={archive.control} name="node" label={t("nodeTable.column.node")} required
        rules={{ required: t("import.local.nodeRequired") }}>
        {field => <NodePicker {...field} aria-label={t("nodeTable.column.node")} filter={{ state: ["online"] }}
          onChange={node => { invalidate(); field.onChange(node); }} />}
      </FormField>
      <FormField control={archive.control} name="path" required
        label={t("nodeTable.localDialog.path")} description={<HelpTip content={t("nodeTable.localDialog.help")} />}
        rules={{ validate: path => isAbsoluteArchivePath(path) || t("import.local.pathRequired") }}>
        {field => (
          <div className="flex items-center gap-2">
            <Input {...field} aria-label={t("nodeTable.localDialog.path")} placeholder="/data/images/runner.tar"
              onChange={event => { invalidate(); field.onChange(event); }} />
            <Button type="button" loading={busy} onClick={() => void resolve()}>{t("import.url.resolve")}</Button>
          </div>
        )}
      </FormField>
      {error && <Alert status="danger"><AlertDescription>{error}</AlertDescription></Alert>}
      {tags && <p role="status">{tags.length ? t("import.local.parsed", { ref: picked }) : t("import.local.noTags")}</p>}
      {tags && tags.length > 1 && (
        <div role="group" aria-label={t("import.local.chooseTag")} className="flex flex-wrap gap-2">
          {tags.map(tag => <Button key={tag} type="button" size="sm" aria-pressed={tag === picked}
            onClick={() => pick(tag, preview)}>{tag}</Button>)}
        </div>
      )}
    </div>
  );
}
