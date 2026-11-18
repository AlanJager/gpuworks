import CatalogDetails from "./CatalogDetails";
import { useCatalogPages } from "./useCatalogPages";
import { CatalogDetailSkeleton, CatalogResultsSkeleton } from "./CatalogSkeleton";
import { OnlineTiles } from "./OnlineTiles";
import { RegisterIconButton } from "./RegisterIconButton";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { allOperationsLabelKey, operationLabelKey, operationTypes } from "../../../modelInference";
import type { OperationType } from "../../../inferenceApi";
import { api, type InferenceTemplate } from "../../../api";
import EmptyState from "../../../components/EmptyState";
import { Badge } from "../../../components/ui/badge";
import { Button } from "../../../components/ui/button";
import { Input } from "../../../components/ui/input";
import { Tabs, TabsList, TabItem } from "../../../components/ui/tabs";
import { createHugeIcon } from "../../../lib/huge-icon";
import LayoutTwoColumnIcon from "@hugeicons/core-free-icons/LayoutTwoColumnIcon";
import GridViewIcon from "@hugeicons/core-free-icons/GridViewIcon";
import { HugeiconsIcon } from "@hugeicons/react";
import Download04Icon from "@hugeicons/core-free-icons/Download04Icon";
import FavouriteIcon from "@hugeicons/core-free-icons/FavouriteIcon";
import { OptionSelect } from "../../../ui-kit/OptionSelect";
import { Segmented } from "../../../ui-kit/Segmented";
import { CatalogAvatar } from "./CatalogAvatar";
import {
  catalogSignal,
  readHub,
  problem,
  providers,
  capabilityTasks,
  purposeLabel,
  type Provider,
  type HubModel,
} from "./hubs";


const sortKeys = [
  { value: "trending", label: "catalog.trending" },
  { value: "downloads", label: "catalog.downloads" },
  { value: "likes", label: "catalog.likes" },
];

const views = [
  { value: "split", name: "catalog.split", icon: createHugeIcon(LayoutTwoColumnIcon) },
  { value: "cards", name: "catalog.cards", icon: createHugeIcon(GridViewIcon) },
];


const idKey = (provider: string, id: string) => provider + "/" + id;
interface CatalogProps {
  active: boolean;
  provider: Provider;
  onProvider: (source: Provider) => void;
  onManual: (source: string, id: string, offline: boolean, revision?: string) => void;
  onQuery: (id: string) => void;
}

export default function OnlineCatalog({active, provider: source, onProvider, onManual, onQuery}: CatalogProps) {
  const { t, i18n } = useTranslation("models");
  const fmt = new Intl.NumberFormat(i18n.language, {notation:"compact", maximumFractionDigits:1});
  const [view, setView] = useState("split");
  const [detailOpen, setDetailOpen] = useState(false);
  const cards = view === "cards";
  const [query, setQuery] = useState(""),
    [capability, setCapability] = useState<OperationType | "all">("all"),
    [sort, setSort] = useState("trending");
  const [selected, setSelected] = useState(""),
    [feedback, setFeedback] = useState("");
  const readmeCache = useRef(new Map<string, string>());
  const [refs, setRefs] = useState<Record<string, string>>({});
  const [known, setKnown] = useState<Array<{source:string; source_id:string; revision?:string; artifact_subfolder?:string}>>([]);
  const [matchFailed, setMatchFailed] = useState(false);
  const explicit = useRef(false), tried = useRef(new Set<string>());
  const {result, error: failure, loading, loadMore, retry} = useCatalogPages(active, source, query, capability, sort);
  const listRef = useRef<HTMLDivElement>(null);
  const moreRef = useRef<HTMLDivElement>(null);
  const model =
    result?.items.find((m) => m.id === selected) || result?.items[0];
  function resetPage() {
    setDetailOpen(false);
    setSelected("");
    setFeedback("");
    explicit.current = true;
  }
  const manual = () =>
    onManual(
      source,
      /^[^/\s]+\/[^/\s]+$/.test(query.trim()) ? query.trim() : "",
      false,
    );
  useEffect(() => {
    if (!active || loading) return;
    if (result) {
      localStorage.setItem("model-catalog-source", source);
      const previous = [...tried.current][0] as Provider | undefined;
      setFeedback(previous && previous !== source ? t("catalog.fallback", {source: providers[previous], other: providers[source]}) : "");
      tried.current.clear();
      explicit.current = true;
    } else if (failure && !explicit.current) {
      tried.current.add(source);
      const other = source === "modelscope" ? "huggingface" : "modelscope";
      if (!tried.current.has(other)) {
        setFeedback(t("catalog.switching", {source: providers[source], other: providers[other]}));
        onProvider(other);
      } else {
        explicit.current = true;
        setFeedback("");
      }
    }
  }, [active, loading, result, failure, source, onProvider, t]);
  useEffect(() => {
    if (!active || loading || failure || !result?.next || !listRef.current || !moreRef.current) return;
    const observer = new IntersectionObserver(entries => {
      if (entries.some(entry => entry.isIntersecting)) loadMore();
    }, {root: listRef.current, rootMargin: "0px 0px 160px 0px"});
    observer.observe(moreRef.current);
    return () => observer.disconnect();
  }, [active, loading, failure, result, cards, loadMore]);
  useEffect(() => { listRef.current?.scrollTo?.(0, 0); }, [source, query, capability, sort]);
  useEffect(() => {
    if (!active) return;
    let alive = true;
    api.models().then((data) => {
      if (alive) setKnown(data.items);
    }).catch(() => {});
    return () => {
      alive = false;
    };
  }, [active]);
  const refFor = (m: HubModel) => m.revision || refs[idKey(source, m.id)] || "";
  const [matches, setMatches] = useState<{key:string; templates:InferenceTemplate[]}>();
  const [matchRetry, setMatchRetry] = useState(0);
  const modelID = active && (!cards || detailOpen) ? model?.id : undefined;
  const revision = model ? refFor(model) : "";
  const matchKey = JSON.stringify([source, modelID, revision]);
  const templates = matches?.key === matchKey ? matches.templates : [];
  useEffect(() => {
    if (!active || !modelID || !revision) return;
    let current = true;

    setMatchFailed(false);
    async function load() {
      const found: InferenceTemplate[] = [];
      for (let page = 1; current; page++) {
        const result = await api.inferenceTemplates({page, page_size: 50, extra: {weight_repo: [modelID!], weight_provider: [source], weight_revision: [revision]}});
        found.push(...result.items);
        if (!result.items.length || found.length >= result.total) break;
      }
      if (current) setMatches({key:matchKey, templates:found});
    }
    load().catch(() => { if (current) setMatchFailed(true); });
    return () => { current = false; };
  }, [active, modelID, source, revision, matchKey, matchRetry]);
  const [revisionErrors, setRevisionErrors] = useState<Record<string, string>>({});
  const revisionIDs = source !== "modelscope" ? [] : result?.items.filter(item =>
    !refFor(item) && !revisionErrors[idKey(source, item.id)] &&
    (item.id === modelID || known.some(m => m.source === source && m.source_id === item.id && !m.artifact_subfolder))
  ).map(item => item.id) ?? [];
  const revisionID = revisionIDs.find(id => id === modelID) || revisionIDs[0];
  useEffect(() => {
    if (!active || !revisionID) return;
    const controller = new AbortController();
    const key = idKey(source, revisionID);
    const url = "https://modelscope.cn/api/v1/models/" + revisionID.split("/").map(encodeURIComponent).join("/") + "/commits?Revision=master&PageSize=1";
    readHub(source, url, catalogSignal(controller.signal, 6000)).then(async response => {
      if (!response.ok) throw new Error(t("catalog.revisionError"));
      const revision = (await response.json()).Data?.Commit?.[0]?.Id;
      if (!/^[a-f0-9]{40}$/.test(revision)) throw new Error(t("catalog.revisionError"));
      if (!controller.signal.aborted) setRefs(prev => ({...prev, [key]: revision}));
    }).catch(cause => {
      if (!controller.signal.aborted) setRevisionErrors(prev => ({...prev, [key]: problem(cause)}));
    });
    return () => controller.abort();
  }, [active, revisionID, source, t]);
  const registration = (m: HubModel) => (
    <RegisterIconButton
      registered={known.some(item => item.source === source && item.source_id === m.id && !item.artifact_subfolder && (item.revision || "") === refFor(m))}
      onClick={() => onManual(source, m.id, false, refFor(m))}
    />
  );
  const details =
    model && active && (!cards || detailOpen) ? (
      <CatalogDetails
        key={idKey(source, model.id)}
        model={model}
        version={refFor(model)}
        source={source}
        readmeCache={readmeCache.current}
        retryMatches={matchFailed ? () => setMatchRetry(v => v + 1) : undefined}
        revisionError={revisionErrors[idKey(source, model.id)]}
        retryRevision={() => setRevisionErrors(prev => ({...prev, [idKey(source, model.id)]: ""}))}
        templates={templates}
        closeDrawer={cards ? () => setDetailOpen(false) : undefined}
      />
    ) : null;
  return (
    <>
      <div className="live-controls">
        <Segmented
          label={t("catalog.source")}
          value={source}
          options={Object.entries(providers).map(([value, label]) => ({
            value,
            label,
          }))}
          onChange={(v) => {
            resetPage();
            onProvider(v as Provider);
          }}
        />
        <OptionSelect
          size="sm"
          aria-label={t(allOperationsLabelKey)}
          className="min-w-24"
          value={capability}
          options={[
            { value: "all", label: t(allOperationsLabelKey) },
            ...operationTypes.filter(type => capabilityTasks[type]).map(type => ({
              value: type, label: t(operationLabelKey[type]),
            })),
          ]}
          onChange={value => {
            resetPage();
            setCapability(value as OperationType | "all");
          }}
        />
        <OptionSelect
          size="sm"
          aria-label={t("catalog.sort")}
          className="min-w-20"
          value={sort}
          options={sortKeys.map(option => ({...option, label:t(option.label)}))}
          onChange={(value) => {
            resetPage();
            setSort(value);
          }}
        />
        <Input
          size="sm"
          value={query}
          aria-label={t("catalog.search")}
          placeholder={t("catalog.searchHint")}
          onChange={(e) => {
            resetPage();
            setQuery(e.target.value);
            onQuery(
              /^[^/\s]+\/[^/\s]+$/.test(e.target.value.trim())
                ? e.target.value.trim()
                : "",
            );
          }}
        />
        <Tabs variant="segment" value={view} onValueChange={setView}>
            <TabsList aria-label={t("catalog.views")}>
              {views.map(({ value, name, icon }) => (
                <TabItem key={value} value={value} icon={icon} label=""
                  aria-label={t(name)} title={t(name)}
                  className="w-control-md justify-center gap-0 px-0" />
              ))}
            </TabsList>
        </Tabs>
      </div>
      {(!result && failure) || result?.items.length === 0 ? (
        <div className="catalog-empty" role="status">
          <EmptyState kind="models" description={<>
            <strong>{failure || t("catalog.noResults")}</strong>
            <span>{failure ? t("catalog.unavailableHelp") : t("catalog.noResultsHelp")}</span>
          </>} action={<>
            {failure ? <Button size="sm" onClick={retry}>{t("catalog.retry")}</Button> :
              (query || capability !== "all") && <Button size="sm" onClick={() => {
                resetPage();
                setQuery("");
                setCapability("all");
                onQuery("");
              }}>{t("catalog.clear")}</Button>}
            <Button size="sm" variant="link" onClick={manual}>{t("catalog.byID")}</Button>
          </>} />
        </div>
      ) : <div className={cards ? "live-grid live-tiles-view" : "live-grid"}>
        <section className="live-browser">
          <div className="live-results" ref={listRef} aria-busy={loading}>
            {!result ? (
              <CatalogResultsSkeleton cards={cards} />
            ) : (
              cards ? (
                <OnlineTiles
                  models={result.items}
                  source={source}
                  taskLabel={purposeLabel}
                  registerButton={registration}
                  onOpen={(id) => {
                    setDetailOpen(true);
                    setSelected(id);
                  }}
                />
              ) : (
                result.items.map((m) => (
                  <article
                    key={m.id}
                    className="live-card"
                    data-selected={model?.id === m.id}
                  >
                    <button
                      className="live-pick"
                      aria-label={m.id}
                      aria-pressed={model?.id === m.id}
                      onClick={() => setSelected(m.id)}
                    >
                      <span className="live-card-title">
                        <CatalogAvatar modelID={m.id} />
                        <b title={m.id}>{m.id.split("/").pop()}</b>
                      </span>
                      <span className="live-card-facts">
                        <small>{m.id.includes("/") ? m.id.split("/")[0] : providers[source]}</small>
                        {[
                          { label: t("catalog.like"), value: m.likes, icon: FavouriteIcon },
                          { label: t("catalog.download"), value: m.downloads, icon: Download04Icon },
                        ].map(({ label, value, icon }) => value != null && (
                          <Badge key={label} size="sm" title={label + " " + value.toLocaleString(i18n.language)}
                            leadingIcon={<HugeiconsIcon icon={icon} aria-hidden />}>
                            <span className="sr-only">{label} </span>{fmt.format(value)}
                          </Badge>
                        ))}
                      </span>
                    </button>
                    {registration(m)}
                  </article>
                ))
              )
            )}
            {result && (
              <div className="catalog-more" ref={moreRef}>
                {failure ? <><span role="status">{failure}</span><Button size="sm" variant="link" onClick={retry}>{t("catalog.retry")}</Button></> :
                  loading ? <span role="status">{t("catalog.loading", {source: providers[source]})}</span> :
                  result.next ? <Button size="sm" variant="ghost" onClick={loadMore}>{t("catalog.more")}</Button> :
                  <span role="status">{t(source === "modelscope" && (result.total ?? 0) > result.items.length ? "catalog.refine" : "catalog.end")}</span>}
              </div>
            )}
          </div>
        </section>
        {!cards && (!result ? <CatalogDetailSkeleton /> : details)}
      </div>}
      {cards && details}
      <div className="registration-feedback" role="status">{feedback}</div>
      <span role="status" className="sr-only">
        {active && !feedback && !result && !failure && t("catalog.loading", {source:providers[source]})}
      </span>
    </>
  );
}
