import { useCallback, useEffect, useState } from "react";
import { Badge, Button, Empty, List, Popover, Space, Tag, Tooltip, Typography } from "antd";
import { BellOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, type NoticeItem, type NoticeList } from "../api";
import { formatTime } from "../i18n/datetime";

const { Text, Paragraph, Link } = Typography;

// How often an open console asks for notices again. The server fetches them
// from the distribution channel with its update check, at most hourly.
const refreshMs = 30 * 60_000;

const severityColor: Record<NoticeItem["severity"], string> = { critical: "red", important: "orange", info: "blue" };

// localized picks the publisher's text for the console's language, falling
// back to English, which every notice carries.
function localized(texts: Record<string, string> | undefined, language: string): string {
  if (!texts) return "";
  const base = language.split("-")[0];
  return texts[language] ?? texts[base] ?? texts.en ?? "";
}

// NoticeBell lists security advisories, releases and events the publisher
// addressed to this installation. Nothing pops up: a badge counts what the
// administrator has not read.
export default function NoticeBell() {
  const { t, i18n } = useTranslation();
  const [list, setList] = useState<NoticeList | null>(null);
  const [failed, setFailed] = useState(false);
  const [open, setOpen] = useState(false);

  const load = useCallback(async () => {
    try {
      setList(await api.notices());
      setFailed(false);
    } catch {
      setFailed(true);
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), refreshMs);
    return () => window.clearInterval(timer);
  }, [load]);

  const markRead = async (ids: string[]) => {
    if (ids.length === 0) return;
    try {
      await api.readNotices(ids);
    } finally {
      await load();
    }
  };

  const unread = list?.unread ?? 0;
  const notices = list?.notices ?? [];
  const content = (
    <div className="notice-bell-panel" style={{ width: 380, maxWidth: "calc(100vw - 32px)" }}>
      {failed && !list && <Text type="secondary">{t("notices.loadFailed")}</Text>}
      {list && notices.length === 0 && <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t("notices.empty")} />}
      {notices.length > 0 && (
        <List<NoticeItem>
          size="small"
          dataSource={notices}
          style={{ maxHeight: 420, overflow: "auto" }}
          renderItem={(notice) => {
            const title = localized(notice.title, i18n.language);
            const body = localized(notice.body, i18n.language);
            return (
              <List.Item
                data-testid={`notice-${notice.id}`}
                style={{ opacity: notice.read ? 0.65 : 1, alignItems: "flex-start" }}
                actions={notice.read ? [] : [
                  <Button key="read" size="small" type="link" onClick={() => void markRead([notice.id])}>{t("notices.markRead")}</Button>,
                ]}
              >
                <Space direction="vertical" size={2} style={{ width: "100%" }}>
                  <Space size={4} wrap>
                    <Tag color={severityColor[notice.severity]}>{t(`notices.severity.${notice.severity}`)}</Tag>
                    <Tag>{t(`notices.kind.${notice.kind}`)}</Tag>
                    <Text type="secondary" style={{ fontSize: 12 }}>{formatTime(notice.published_at)}</Text>
                  </Space>
                  <Text strong={!notice.read}>{title}</Text>
                  {body && <Paragraph type="secondary" style={{ marginBottom: 0, whiteSpace: "pre-wrap" }}>{body}</Paragraph>}
                  {notice.url && (
                    <Link href={notice.url} target="_blank" rel="noopener noreferrer">{t("notices.readMore")}</Link>
                  )}
                </Space>
              </List.Item>
            );
          }}
        />
      )}
    </div>
  );

  return (
    <Popover
      open={open}
      onOpenChange={(next) => { setOpen(next); if (next) void load(); }}
      trigger="click"
      placement="bottomRight"
      title={(
        <Space style={{ width: "100%", justifyContent: "space-between" }}>
          <span>{t("notices.title")}</span>
          {unread > 0 && (
            <Button size="small" type="link" onClick={() => void markRead(notices.filter((n) => !n.read).map((n) => n.id))}>
              {t("notices.markAllRead")}
            </Button>
          )}
        </Space>
      )}
      content={content}
    >
      <Tooltip title={unread > 0 ? t("notices.unread", { count: unread }) : t("notices.title")}>
        <Button type="text" aria-label={unread > 0 ? `${t("notices.title")} (${t("notices.unread", { count: unread })})` : t("notices.title")}
          icon={<Badge count={unread} size="small" offset={[2, -2]}><BellOutlined /></Badge>} />
      </Tooltip>
    </Popover>
  );
}
