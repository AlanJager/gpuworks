import { Alert, Button, Card, Empty, Input, Space, Spin, Table, Tag, Tooltip, Typography } from "antd";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type CommunityRepo, type CommunityRepoBrowse, type CommunityTag } from "../../api";
import { formatTime } from "../../i18n/datetime";
import { searchCommunityTags } from "./tagFilter";

const { Text } = Typography;

/** What choosing an upstream tag fills into the image form. */
export type CommunityPick = {
  imageRef: string;
  engine: string;
  framework: string;
  engineVersion: string;
  rollingTag: boolean;
};

type Props = {
  onPick: (pick: CommunityPick) => void;
};

type Browse =
  | { state: "idle" }
  | { state: "loading"; repo: string }
  | { state: "error"; repo: string; message: string }
  | { state: "done"; repo: string; result: CommunityRepoBrowse };

/**
 * ImageCommunityBrowser lists the built-in community repositories as cards
 * and every tag upstream holds for the one chosen — release, nightly and
 * model-specific tags alike — with a name search instead of a hidden filter.
 * Any other repository can be typed in and browsed the same way.
 */
export default function ImageCommunityBrowser({ onPick }: Props) {
  const { t } = useTranslation("images");
  const [repos, setRepos] = useState<CommunityRepo[] | null>(null);
  const [reposError, setReposError] = useState("");
  const [manualRepo, setManualRepo] = useState("");
  const [browse, setBrowse] = useState<Browse>({ state: "idle" });
  const [query, setQuery] = useState("");
  const [picked, setPicked] = useState("");

  useEffect(() => {
    let alive = true;
    api
      .communityRepos()
      .then((rows) => alive && setRepos(rows.items))
      .catch((e) => {
        if (!alive) return;
        setRepos([]);
        setReposError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      alive = false;
    };
  }, []);

  const open = async (repo: string, card?: CommunityRepo) => {
    const target = repo.trim();
    if (!target) return;
    setBrowse({ state: "loading", repo: target });
    setQuery("");
    try {
      const result = await api.browseCommunity(target);
      // A sibling path lists its own tags but belongs to its card, so the
      // card's engine and framework stand in when the answer leaves them out.
      setBrowse({
        state: "done",
        repo: target,
        result: {
          ...result,
          engine: result.engine || card?.engine || "",
          framework: result.framework || card?.framework || "",
        },
      });
    } catch (e) {
      setBrowse({ state: "error", repo: target, message: e instanceof Error ? e.message : String(e) });
    }
  };

  const visible = useMemo(
    () => (browse.state === "done" ? searchCommunityTags(browse.result.tags ?? [], query) : []),
    [browse, query],
  );

  const pick = (result: CommunityRepoBrowse, tag: CommunityTag) => {
    const imageRef = `${result.repo || (browse.state === "done" ? browse.repo : "")}:${tag.tag}`;
    setPicked(imageRef);
    onPick({
      imageRef,
      engine: result.engine,
      framework: result.framework,
      engineVersion: tag.engine_version || "",
      rollingTag: Boolean(tag.rolling_tag),
    });
  };

  const activeRepo = browse.state === "idle" ? "" : browse.repo;

  return (
    <Card size="small" title={t("import.community.title")}>
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        {repos === null ? (
          <Space>
            <Spin size="small" />
            <Text type="secondary">{t("import.community.reposLoading")}</Text>
          </Space>
        ) : reposError ? (
          <Alert type="error" showIcon message={t("import.community.reposFailed")} description={reposError} />
        ) : (
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(220px, 1fr))", gap: 8 }}>
            {repos.map((card) => (
              <Card key={card.repo} size="small" styles={{ body: { padding: 8 } }}>
                <Space direction="vertical" size={2} style={{ width: "100%" }}>
                  {[card.repo, ...(card.siblings ?? [])].map((path) => (
                    <Button
                      key={path}
                      type={activeRepo === path ? "primary" : "link"}
                      size="small"
                      style={{ padding: activeRepo === path ? undefined : 0, maxWidth: "100%", whiteSpace: "normal", textAlign: "left", height: "auto" }}
                      onClick={() => void open(path, card)}
                    >
                      {path}
                    </Button>
                  ))}
                  <Space size={4}>
                    {card.engine && <Tag color="blue">{card.engine}</Tag>}
                    {card.framework && <Tag>{card.framework}</Tag>}
                  </Space>
                </Space>
              </Card>
            ))}
          </div>
        )}
        <Space.Compact style={{ width: "100%" }}>
          <Input
            id="community-repository"
            name="community_repository"
            aria-label={t("import.community.otherRepo")}
            placeholder={t("import.community.otherRepoPlaceholder")}
            value={manualRepo}
            onChange={(event) => setManualRepo(event.target.value)}
            onPressEnter={() => void open(manualRepo)}
          />
          <Button onClick={() => void open(manualRepo)} disabled={!manualRepo.trim()}>
            {t("import.community.browse")}
          </Button>
        </Space.Compact>
        {browse.state === "loading" && (
          <Space>
            <Spin size="small" />
            <Text type="secondary">{t("import.community.tagsLoading", { repo: browse.repo })}</Text>
          </Space>
        )}
        {browse.state === "error" && (
          <Alert
            type="error"
            showIcon
            message={t("import.community.tagsFailed", { repo: browse.repo })}
            description={browse.message}
          />
        )}
        {browse.state === "done" && (
          <Space direction="vertical" size={8} style={{ width: "100%" }}>
            <Text strong>{t("import.community.tagCount", { repo: browse.repo, total: browse.result.tags?.length ?? 0 })}</Text>
            <Input
              allowClear
              aria-label={t("import.community.search")}
              placeholder={t("import.community.searchPlaceholder")}
              value={query}
              onChange={(event) => setQuery(event.target.value)}
            />
            <Table
              rowKey="tag"
              size="small"
              tableLayout="fixed"
              scroll={{ x: 560 }}
              dataSource={visible}
              pagination={{ pageSize: 10, hideOnSinglePage: true, showSizeChanger: false }}
              locale={{
                emptyText: (
                  <Empty
                    image={Empty.PRESENTED_IMAGE_SIMPLE}
                    description={
                      (browse.result.tags?.length ?? 0) === 0
                        ? t("import.community.noTags")
                        : t("import.community.noMatch")
                    }
                  />
                ),
              }}
              columns={[
                {
                  title: t("import.community.column.tag"),
                  key: "tag",
                  width: 240,
                  render: (_: unknown, tag: CommunityTag) => (
                    <Tooltip title={tag.tag} trigger={["hover", "focus"]}>
                      <Button
                        type={picked === `${browse.result.repo || browse.repo}:${tag.tag}` ? "primary" : "link"}
                        size="small"
                        style={{ paddingInline: 4, maxWidth: "100%" }}
                        onClick={() => pick(browse.result, tag)}
                      >
                        <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{tag.tag}</span>
                      </Button>
                    </Tooltip>
                  ),
                },
                {
                  title: t("import.community.column.labels"),
                  key: "labels",
                  width: 152,
                  render: (_: unknown, tag: CommunityTag) => (
                    <>
                      {tag.rolling_tag && <Tag color="gold">{t("import.rolling")}</Tag>}
                      {tag.upstream_gone && <Tag color="red">{t("tag.upstreamGone")}</Tag>}
                      {tag.engine_version && <Text type="secondary">{tag.engine_version}</Text>}
                    </>
                  ),
                },
                {
                  title: t("import.community.column.lastUpdated"),
                  key: "last_updated",
                  width: 168,
                  render: (_: unknown, tag: CommunityTag) => (
                    <Text type="secondary" style={{ whiteSpace: "nowrap" }}>{formatTime(tag.last_updated)}</Text>
                  ),
                },
              ]}
            />
          </Space>
        )}
      </Space>
    </Card>
  );
}
