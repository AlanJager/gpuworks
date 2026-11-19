import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { DoctorDisk, DoctorFact, DoctorFinding, DoctorMount, DoctorRunIdentity, DoctorTarget, NodeDoctorReport as Report } from "../api";
import { GPUHealthDetail, GPUHealthSummary, HealthCommand, HealthEvidence, gpuHealthLabel } from "./GPUHealth";
import NodeDoctorChecks from "./NodeDoctorChecks";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "./ui/accordion";
import { Badge, type BadgeStatus } from "./ui/badge";
import { DetailList, DetailListItem, DetailListLabel, DetailListValue } from "./ui/detail-list";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { Notice } from "../ui-kit/Notice";
import { runtimeLabel } from "../nodeDisplay";

const rawStyle = {
  background: "#111827",
  color: "#f9fafb",
  padding: 12,
  borderRadius: 2,
  maxHeight: 320,
  overflow: "auto",
  whiteSpace: "pre-wrap",
  margin: 0,
} as const;

const healthStatus: Record<string, BadgeStatus> = { OK: "success", Warning: "warning", Error: "danger" };

function Fact({ label, children }: { label: ReactNode; children: ReactNode }) {
  return <DetailListItem><DetailListLabel>{label}</DetailListLabel><DetailListValue>{children}</DetailListValue></DetailListItem>;
}

function SectionTitle({ children }: { children: ReactNode }) {
  return <h3 className="mb-2 text-body font-semibold">{children}</h3>;
}

function gib(bytes: number): string {
  return `${(bytes / 1024 ** 3).toFixed(1)} GiB`;
}

function percent(part: number, total: number): number {
  return total > 0 ? Math.floor((part * 100) / total) : 0;
}

function FindingTarget({ finding, target, status }: { finding: DoctorFinding; target: DoctorTarget; status: "Warning" | "Error" }) {
  const { t } = useTranslation("nodes");
  const resettable = !!target.actions?.length;
  return (
    <div className="node-doctor-target">
      <span className="font-semibold">{t("doctor.card", { index: target.index, bdf: target.pcie_bdf || "—" })}</span>
      <GPUHealthDetail
        status={status}
        code={finding.code}
        evidence={target.evidence}
        guide={target.probe ? { probe: target.probe, recheck: target.recheck, actions: target.actions } : undefined}
        commandsOnly
        extraAction={resettable && (
          <div className="flex flex-col gap-0.5">
            {target.processes?.map((proc) => (
              <span key={proc.pid} className="text-fg-danger">{t("doctor.runningProcess", { pid: proc.pid, name: proc.name })}</span>
            ))}
          </div>
        )}
      />
    </div>
  );
}

function AcceleratorFinding({ finding }: { finding: DoctorFinding }) {
  const { t } = useTranslation("nodes");
  const { t: tc } = useTranslation("common");
  const status = finding.status === "fail" ? "Error" : "Warning";
  const targets = finding.targets ?? [];
  return (
    <div className="node-doctor-finding flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <Badge status={status === "Error" ? "danger" : "warning"}>{gpuHealthLabel(tc, status, finding.code)}</Badge>
        <span>{t("doctor.cards", { count: targets.length, indexes: targets.map((target) => target.index).join(", ") })}</span>
      </div>
      <GPUHealthSummary status={status} code={finding.code} withAction />
      {targets.map((target) => <FindingTarget key={target.index} finding={finding} target={target} status={status} />)}
    </div>
  );
}

const nodeFindingCodes = new Set([
  "DISK_LOW", "RUNTIME_UNAVAILABLE", "RUNTIME_PROVISION_FAILED", "RUNTIME_STATE_UNTRAVERSABLE", "PATH_NOEXEC", "PATH_READ_ONLY", "PATH_EPHEMERAL",
  "ACCELERATOR_PROBE_FAILED", "ACCELERATOR_UNRECOGNIZED", "CUDA_INIT_FAILED", "MULTI_GPU_CHECK_FAILED", "MACOS_UNSUPPORTED", "MACOS_IDLE_SLEEP",
  "PEER_MEMORY_MISSING", "UCX_NETLINK_HANG",
  // Why a check did not run.
  "MOUNT_TABLE_UNREADABLE", "CUDA_CHECK_NOT_PASSED", "CHECK_PREREQUISITE_MISSING", "CHECK_TOOL_FAILED", "CHECK_CANCELLED", "CHECK_BUDGET_EXHAUSTED",
  "CARD_UNADDRESSABLE",
]);

const findingStatus: Record<DoctorFinding["status"], BadgeStatus> = { fail: "danger", warn: "warning", skip: "neutral" };

/** A finding's name in the reader's language: the node finding, a card check's skip reason, or an accelerator health code. */
export function doctorFindingLabel(t: TFunction<"nodes">, tc: TFunction<"common">, finding: DoctorFinding): string {
  if (nodeFindingCodes.has(finding.code)) return t(`doctor.finding.${finding.code}.label`);
  if (finding.status === "skip") return t("doctor.notChecked", { reason: t(`cardHealth.skip.${finding.code}`, { defaultValue: finding.code }) });
  return gpuHealthLabel(tc, finding.status === "fail" ? "Error" : "Warning", finding.code);
}

/** A check that did not run on some cards: why, and which cards, never a verdict about them. */
function SkippedCardsFinding({ finding }: { finding: DoctorFinding }) {
  const { t } = useTranslation("nodes");
  const { t: tc } = useTranslation("common");
  const targets = finding.targets ?? [];
  return (
    <div className="node-doctor-finding flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <Badge status="neutral">{doctorFindingLabel(t, tc, finding)}</Badge>
        <span>{t("doctor.cards", { count: targets.length, indexes: targets.map((target) => target.index).join(", ") })}</span>
      </div>
      {nodeFindingCodes.has(finding.code) && <p className="m-0">{t(`doctor.finding.${finding.code}.summary`)}</p>}
      <HealthEvidence fields={finding.evidence} />
    </div>
  );
}

/** Renders a finding by what it is: a check that did not run, a card health finding, or a node finding. */
export function DoctorFindingView({ finding }: { finding: DoctorFinding }) {
  if (finding.status === "skip" && finding.targets?.length) return <SkippedCardsFinding finding={finding} />;
  if (finding.targets?.length) return <AcceleratorFinding finding={finding} />;
  return <NodeFinding finding={finding} />;
}

/** Filesystem type and the mount flags that matter to the reader. */
function MountTags({ mount }: { mount?: DoctorMount }) {
  const { t } = useTranslation("nodes");
  if (!mount) return null;
  const flags = [
    mount.network && "network", mount.noexec && "noexec", mount.read_only && "readOnly", mount.ephemeral && "ephemeral",
  ].filter((flag): flag is string => !!flag);
  return (
    <>
      <span className="text-fg-muted">{mount.fs_type}</span>
      {flags.map((flag) => <Badge size="sm" key={flag} color={flag === "network" ? "blue" : "orange"}>{t(`doctor.mountFlag.${flag}`)}</Badge>)}
    </>
  );
}

function evidenceValue(finding: DoctorFinding, name: string): string {
  return finding.evidence?.find((field) => field.name === name)?.value ?? "";
}

/** Values a finding's sentence interpolates from its evidence rather than a disk. */
function evidenceParams(finding: DoctorFinding) {
  return {
    required: evidenceValue(finding, "required"),
    directory: evidenceValue(finding, "directory"),
    user: evidenceValue(finding, "user"),
    stateDir: evidenceValue(finding, "state_dir"),
    module: evidenceValue(finding, "module"),
    fix: evidenceValue(finding, "fix"),
  };
}

function NodeFinding({ finding }: { finding: DoctorFinding }) {
  const { t } = useTranslation("nodes");
  const key = nodeFindingCodes.has(finding.code) ? `doctor.finding.${finding.code}` : "";
  const impact = key ? t(`${key}.impact`, { ...evidenceParams(finding), defaultValue: "" }) : "";
  const disk = finding.disk;
  return (
    <div className="node-doctor-finding flex flex-col gap-2">
      <Badge status={findingStatus[finding.status] ?? "danger"}>
        {key ? t(`${key}.label`) : finding.code}
      </Badge>
      {key && (
        <span>
          {t(`${key}.summary`, disk ? {
            path: disk.path, avail: gib(disk.avail_bytes), total: gib(disk.total_bytes), percent: percent(disk.avail_bytes, disk.total_bytes),
            fstype: disk.mount?.fs_type ?? "", point: disk.mount?.point ?? "",
          } : evidenceParams(finding))}
        </span>
      )}
      {impact && (
        <div className="gpu-health-section">
          <span className="font-semibold text-fg-muted">{t("doctor.impact")}</span>
          <span>{impact}</span>
        </div>
      )}
      {(finding.probe || !!finding.evidence?.length) && (
        <div className="gpu-health-section">
          <span className="font-semibold text-fg-muted">{t("doctor.evidence")}</span>
          {finding.probe && <HealthCommand command={finding.probe} />}
          <HealthEvidence fields={finding.evidence} />
        </div>
      )}
      <div className="gpu-health-section">
        <span className="font-semibold text-fg-muted">{t("doctor.recheck")}</span>
        {finding.recheck ? <HealthCommand command={finding.recheck} /> : <span>{t("doctor.rerun")}</span>}
      </div>
    </div>
  );
}

function factValue(t: TFunction<"nodes">, fact: DoctorFact): string {
  const { code, value } = fact;
  if (code === "runtime_plugin" && value === "none") return t("doctor.pluginNone");
  if (code === "device_injection" && value === "explicit") return t("doctor.injectionExplicit");
  if (code === "runtime_identity" && fact.identity) return identityText(t, fact.identity);
  if (code === "network_filesystem" && fact.disks?.length) return networkFilesystemText(t, fact.disks);
  return value;
}

function identityText(t: TFunction<"nodes">, identity: DoctorRunIdentity): string {
  if (identity.pending) return t("doctor.identity.pending", { runtime: identity.runtime });
  if (identity.user) return t("doctor.identity.dedicated", { user: identity.user, uid: identity.uid });
  if (identity.fallback) return t("doctor.identity.fallback", { uid: identity.uid, reason: identity.fallback });
  return t("doctor.identity.own", { uid: identity.uid });
}

function networkFilesystemText(t: TFunction<"nodes">, disks: DoctorDisk[]): string {
  const mount = disks[0].mount;
  const paths = disks.map((disk) => `${t(`doctor.pathKind.${disk.kind}`, { defaultValue: disk.kind })} ${disk.path}`);
  return t(mount?.source ? "doctor.networkFilesystemFrom" : "doctor.networkFilesystem", {
    point: mount?.point ?? "", fstype: mount?.fs_type ?? "", source: mount?.source ?? "", paths: paths.join(t("doctor.listSeparator")),
  });
}

/** The agent's raw text, rendered only while expanded: it can be long. */
function RawReport({ text, label }: { text: string; label: string }) {
  const [open, setOpen] = useState("");
  return (
    <Accordion value={open} onValueChange={(value: string) => setOpen(value)}>
      <AccordionItem value="raw">
        <AccordionTrigger>{label}</AccordionTrigger>
        <AccordionContent>{open === "raw" && <pre style={rawStyle}>{text}</pre>}</AccordionContent>
      </AccordionItem>
    </Accordion>
  );
}

/**
 * NodeDoctorReport renders the unified check list and hardware details.
 * Reports from older Servers still render their original findings or text.
 */
export default function NodeDoctorReport({ report }: { report: Report }) {
  const { t } = useTranslation("nodes");
  const { t: tc } = useTranslation("common");
  if (!report.host && !report.checks) {
    return (
      <div className="flex flex-col gap-2">
        <Notice tone="info" title={t("doctor.legacy")} />
        <pre style={rawStyle}>{report.report}</pre>
      </div>
    );
  }
  const host = report.host;
  const accel = report.accelerators;
  const findings = report.findings ?? [];
  const counts: Record<string, number> = { OK: 0, Warning: 0, Error: 0 };
  for (const gpu of accel?.gpus ?? []) counts[gpu.health_status] = (counts[gpu.health_status] ?? 0) + 1;
  return (
    <div className="flex flex-col gap-4">
      {report.checks && <NodeDoctorChecks report={report} renderFinding={(finding) => <DoctorFindingView finding={finding} />}
        findingLabel={(finding) => doctorFindingLabel(t, tc, finding)} />}
      {host && <section aria-label={t("doctor.overview")}>
        <SectionTitle>{t("doctor.overview")}</SectionTitle>
        <DetailList>
          <Fact label={t("doctor.host")}>{`${host.hostname} · ${host.os}/${host.arch} · kernel ${host.kernel}`}</Fact>
          <Fact label={t("doctor.cpu")}>{`${host.cpu_model} × ${host.cpu_cores}`}</Fact>
          <Fact label={t("doctor.memory")}>{t("doctor.memoryValue", { total: gib(host.mem_total_bytes), avail: gib(host.mem_avail_bytes) })}</Fact>
          <Fact label={t("doctor.disk")}>
            <div className="flex flex-col items-end">
              {host.disks.map((disk) => (
                <span key={`${disk.kind}-${disk.path}`} className="inline-flex flex-wrap items-center justify-end gap-1">
                  <span>
                    {t("doctor.diskValue", { path: disk.path, total: gib(disk.total_bytes), avail: gib(disk.avail_bytes), percent: percent(disk.avail_bytes, disk.total_bytes) })}
                  </span>
                  <MountTags mount={disk.mount} />
                </span>
              ))}
            </div>
          </Fact>
        </DetailList>
      </section>}

      {(host || report.runtimes?.length) && <section aria-label={t("doctor.runtimes")}>
        <SectionTitle>{t("doctor.runtimes")}</SectionTitle>
        <DetailList>
          {(report.runtimes ?? []).map((runtime) => (
            <Fact key={runtime.name} label={runtimeLabel(runtime.name)}>
              <span className="inline-flex flex-wrap items-center justify-end gap-2">
                <span className={runtime.status === "ok" ? undefined : "text-fg-muted"}>
                  {runtime.status === "not_found" ? t("doctor.runtimeNotFound") : runtime.status === "query_failed" ? t("doctor.runtimeQueryFailed") : runtime.version || "—"}
                </span>
                {runtime.selected && <Badge size="sm" color="blue">{t("doctor.runtimeSelected")}</Badge>}
              </span>
            </Fact>
          ))}
        </DetailList>
      </section>}

      {(host || accel) && <section aria-label={t("doctor.accelerators")}>
        <SectionTitle>{t("doctor.accelerators")}</SectionTitle>
        {!accel ? (
          <span className="text-fg-muted">{t("doctor.noAccelerator")}</span>
        ) : (
          <div className="flex flex-col gap-2">
            <span>
              {t("doctor.acceleratorSummary", { vendor: accel.vendor, driver: accel.driver || "—", total: accel.gpus.length, ok: counts.OK, warning: counts.Warning, error: counts.Error })}
            </span>
            <Table containerClassName="overflow-x-auto">
              <TableHeader>
                <TableRow>
                  <TableHead style={{ width: 48 }}>{t("doctor.column.index")}</TableHead>
                  <TableHead>{t("doctor.column.name")}</TableHead>
                  <TableHead>{t("doctor.column.memory")}</TableHead>
                  <TableHead>{t("doctor.column.bdf")}</TableHead>
                  <TableHead>{t("doctor.column.numa")}</TableHead>
                  <TableHead>{t("doctor.column.health")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {accel.gpus.map((gpu) => (
                  <TableRow key={gpu.index}>
                    <TableCell>{gpu.index}</TableCell>
                    <TableCell>{gpu.name}</TableCell>
                    <TableCell>{gib(gpu.memory_bytes)}</TableCell>
                    <TableCell>{gpu.pcie_bdf || "—"}</TableCell>
                    <TableCell>{gpu.numa_node}</TableCell>
                    <TableCell>
                      <Badge status={healthStatus[gpu.health_status] ?? "danger"}>{gpuHealthLabel(tc, gpu.health_status in healthStatus ? gpu.health_status : "Error", gpu.health_code)}</Badge>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </section>}

      {!report.checks && <section aria-label={t("doctor.findings")}>
        <SectionTitle>{t("doctor.findings")}</SectionTitle>
        {findings.length === 0 ? (
          <span className="text-fg-muted">{t("doctor.noFindings")}</span>
        ) : (
          <div className="flex flex-col gap-3">
            {findings.map((finding, i) => <DoctorFindingView key={`${finding.code}-${i}`} finding={finding} />)}
          </div>
        )}
      </section>}

      {!!report.facts?.length && (
        <section aria-label={t("doctor.facts")}>
          <SectionTitle>{t("doctor.facts")}</SectionTitle>
          <DetailList>
            {report.facts.map((fact, i) => (
              <Fact key={`${fact.code}-${i}`} label={t(`doctor.fact.${fact.code}`, { defaultValue: fact.code })}>
                <span className="whitespace-pre-wrap">{factValue(t, fact)}</span>
              </Fact>
            ))}
          </DetailList>
        </section>
      )}

      <RawReport text={report.report} label={t("doctor.rawToggle")} />
    </div>
  );
}
