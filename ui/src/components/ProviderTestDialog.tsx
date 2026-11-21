import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { ConnectionTestInterrupted, type ConnectionCheckEvent, type ConnectionTestSummary } from "../api";
import { useIcons } from "../lib/icon-context";
import { Notice } from "../ui-kit/Notice";
import { Spinner } from "../ui-kit/Spinner";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "./ui/dialog";

/** A connection test as the owner starts it: the streamed checks go to onCheck, the summary settles the promise, and aborting the signal ends the request. */
export type ProviderTestRun = (onCheck: (check: ConnectionCheckEvent) => void, signal: AbortSignal) => Promise<ConnectionTestSummary>;

/** What the dialog runs while nothing was asked for: never started, since it renders only behind open=false. */
export const NOOP_TEST_RUN: ProviderTestRun = () => Promise.resolve({ ok: false });

/**
 * The "test connection" run of a platform account: each check as it streams
 * in, then the summary. A failed check says what the server said of it (the
 * product error code in the reader's language) and what to do about it; a
 * skipped one says why it was not needed. Closing the dialog ends the request.
 */
export default function ProviderTestDialog({ open, run, onClose }: { open: boolean; run: ProviderTestRun; onClose: () => void }) {
  const { t } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const [checks, setChecks] = useState<ConnectionCheckEvent[]>([]);
  const [summary, setSummary] = useState<ConnectionTestSummary | null>(null);
  // Why no summary came: the server's refusal in the reader's language, or true for a stream that broke.
  const [broken, setBroken] = useState<string | true | null>(null);

  useEffect(() => {
    if (!open) return;
    setChecks([]);
    setSummary(null);
    setBroken(null);
    // Closing or unmounting ends the request, so the server stops testing for nobody; what still arrives is dropped.
    const controller = new AbortController();
    const live = () => !controller.signal.aborted;
    run((check) => {
      if (!live()) return;
      // A check whose name arrives again (a retry) replaces its earlier self; seq keeps the server's order.
      setChecks((prev) => [...prev.filter((p) => p.name !== check.name), check].sort((a, b) => a.seq - b.seq));
    }, controller.signal)
      .then((result) => {
        if (!live()) return;
        setSummary(result);
      })
      .catch((error) => {
        if (!live()) return;
        // No summary came: that is a failed run, not a hanging one. A refused request says why in the reader's
        // language; a stream that broke has nothing of the server's to show.
        setSummary({ ok: false });
        setBroken(error instanceof ConnectionTestInterrupted ? true : (error as Error).message);
      });
    return () => controller.abort();
  }, [open, run]);

  // The test could not run at all: the summary carries the product error code. One this console has no words for leaves the title alone.
  const reason = broken === true ? t("test.interrupted")
    : broken || (summary?.code ? tCommon(`apiError.${summary.code}`, { defaultValue: "" }) : "");

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) onClose(); }}>
      <DialogContent size="lg" closeButtonLabel={tCommon("actions.close")} className="flex max-h-[calc(100dvh-32px)] flex-col gap-4">
        <DialogHeader>
          <DialogTitle>{t("test.title")}</DialogTitle>
        </DialogHeader>
        <div className="-mx-1 flex min-h-0 flex-col gap-3 overflow-auto px-1 pb-1">
          {checks.length > 0 && (
            <ul className="m-0 flex list-none flex-col gap-2.5 p-0">
              {checks.map((check) => <CheckRow key={check.name} check={check} />)}
            </ul>
          )}
          {!summary && <Spinner label={t("test.running")} />}
          {summary && (
            // Zeron alerts have no success tone; a passed run reads as information.
            <Notice tone={summary.ok ? "info" : "danger"} title={summary.ok ? t("test.ok") : t("test.failed")}>
              {!summary.ok && reason ? reason : undefined}
            </Notice>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}

function CheckRow({ check }: { check: ConnectionCheckEvent }) {
  const { t } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const icons = useIcons();
  const skipped = check.status === "skipped";
  const Icon = check.status === "ok" ? icons.check : check.status === "failed" ? icons["circle-x"] : icons.circle;
  const tone = check.status === "ok" ? "text-fg-success" : check.status === "failed" ? "text-fg-danger" : "text-fg-muted";
  const failure = check.status === "failed" && check.code
    ? tCommon(`apiError.${check.code}`, { ...(check.detail ?? {}), defaultValue: check.code })
    : "";
  const hint = check.hint ? t(`test.hint.${check.hint}`, { defaultValue: "" }) : "";
  return (
    <li className="flex items-start gap-2">
      <Icon size={16} strokeWidth={2} aria-hidden="true" data-status={check.status} className={`mt-0.5 flex-none ${tone}`} />
      <div className="flex min-w-0 flex-col gap-0.5">
        <span className="flex flex-wrap items-baseline gap-x-2">
          <span className={`text-body ${skipped ? "text-fg-muted" : "text-fg-default"}`}>{t(`test.check.${check.name}`, { defaultValue: check.name })}</span>
          {/* A check that did not run took no time: it says so in words. The other two say it to a screen reader, which has no icon. */}
          {skipped
            ? <span className="text-label text-fg-subtle">{t("test.status.skipped")}</span>
            : <>
              <span className="sr-only">{t(`test.status.${check.status}`)}</span>
              <span className="text-label text-fg-subtle">{check.duration_ms}ms</span>
            </>}
        </span>
        {failure && <span className="text-label text-fg-danger">{failure}</span>}
        {hint && <span className="text-label text-fg-subtle">{hint}</span>}
        {check.status === "ok" && <ProxyNote detail={check.detail} />}
      </div>
    </li>
  );
}

/** What the proxy check found, when the server runs behind one: the variables that set it, and whether requests to this account go through it. */
function ProxyNote({ detail }: { detail?: Record<string, unknown> }) {
  const { t } = useTranslation("providers");
  if (detail?.proxy !== "env" || typeof detail.used !== "boolean") return null;
  const variables = Array.isArray(detail.variables) ? detail.variables.map(String).join(", ") : "";
  if (!variables) return null;
  return <span className="text-label text-fg-subtle">{t(detail.used ? "test.proxy.used" : "test.proxy.unused", { variables })}</span>;
}
