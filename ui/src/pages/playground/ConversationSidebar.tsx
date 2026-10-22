import { DeleteOutlined, EditOutlined, LoadingOutlined, MenuFoldOutlined, MenuUnfoldOutlined, MoreOutlined, PlusOutlined } from "@ant-design/icons";
import { Button, Dropdown, Input, Modal, Tooltip, Typography } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { MAX_CONVERSATIONS, streamingMessages, type Conversation } from "./conversations/model";
import type { ConversationStore } from "./conversations/store";
import type { PlaygroundModel } from "./types";

const DAY_MS = 24 * 60 * 60 * 1000;

function dayGroup(updatedAt: number, now: Date): "today" | "yesterday" | "earlier" {
  const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  if (updatedAt >= midnight) return "today";
  if (updatedAt >= midnight - DAY_MS) return "yesterday";
  return "earlier";
}

export default function ConversationSidebar({
  store,
  conversations,
  currentID,
  persistent,
  models,
  collapsed,
  onCollapse,
}: {
  store: ConversationStore;
  conversations: Conversation[];
  currentID?: string;
  persistent: boolean;
  models: PlaygroundModel[];
  collapsed: boolean;
  onCollapse: (collapsed: boolean) => void;
}) {
  const { t } = useTranslation("playground");
  const [modal, modalContext] = Modal.useModal();
  const [renaming, setRenaming] = useState<{ id: string; title: string }>();
  const generating = conversations.filter((conversation) => streamingMessages(conversation).length).length;
  const labelOf = (id: string) => models.find((model) => model.id === id)?.label || id;
  const titleOf = (conversation: Conversation) => conversation.title || t("conversations.untitled");
  const subtitle = (conversation: Conversation) => conversation.mode === "compare"
    ? t("conversations.compareColumns", { count: conversation.columns.length })
    : labelOf(conversation.columns[0]?.model ?? "");
  const confirmDelete = (conversation: Conversation) => {
    void modal.confirm({
      title: t("conversations.deleteTitle"),
      content: t("conversations.deleteContent", { title: titleOf(conversation) }),
      okText: t("conversations.delete"),
      okButtonProps: { danger: true },
      cancelText: t("conversations.cancel"),
      onOk: () => store.remove(conversation.id),
    });
  };
  const commitRename = () => {
    if (renaming) store.rename(renaming.id, renaming.title);
    setRenaming(undefined);
  };

  if (collapsed) {
    return (
      <aside className="playground-conversations is-collapsed" aria-label={t("conversations.list")}>
        <Tooltip title={t("conversations.new")} placement="right"><Button type="text" icon={<PlusOutlined />} onClick={() => store.newConversation()} aria-label={t("conversations.new")} /></Tooltip>
        <Tooltip title={t("conversations.expand")} placement="right"><Button type="text" icon={<MenuUnfoldOutlined />} onClick={() => onCollapse(false)} aria-label={t("conversations.expand")} /></Tooltip>
        {!!generating && <span className="playground-conversations-badge" title={t("conversations.generatingCount", { count: generating })}><LoadingOutlined /> {generating}</span>}
        {modalContext}
      </aside>
    );
  }

  const now = new Date();
  const groups = (["today", "yesterday", "earlier"] as const)
    .map((group) => ({ group, items: conversations.filter((conversation) => dayGroup(conversation.updatedAt, now) === group) }))
    .filter(({ items }) => items.length);
  return (
    <aside className="playground-conversations" aria-label={t("conversations.list")}>
      <div className="playground-conversations-head">
        <Button type="primary" icon={<PlusOutlined />} onClick={() => store.newConversation()}>{t("conversations.new")}</Button>
        <Tooltip title={t("conversations.collapse")}><Button type="text" icon={<MenuFoldOutlined />} onClick={() => onCollapse(true)} aria-label={t("conversations.collapse")} /></Tooltip>
      </div>
      <div className="playground-conversations-list">
        {groups.map(({ group, items }) => (
          <section key={group} aria-label={t(`conversations.${group}`)}>
            <div className="playground-conversations-group">{t(`conversations.${group}`)}</div>
            {items.map((conversation) => {
              const selected = conversation.id === currentID;
              const busy = streamingMessages(conversation).length > 0;
              if (renaming?.id === conversation.id) {
                return (
                  <div key={conversation.id} className="playground-conversation is-selected">
                    <Input
                      size="small"
                      autoFocus
                      aria-label={t("conversations.titleAria")}
                      value={renaming.title}
                      maxLength={80}
                      onChange={(event) => setRenaming({ id: conversation.id, title: event.target.value })}
                      onPressEnter={commitRename}
                      onBlur={commitRename}
                      onKeyDown={(event) => { if (event.key === "Escape") setRenaming(undefined); }}
                    />
                  </div>
                );
              }
              return (
                <div key={conversation.id} className={`playground-conversation${selected ? " is-selected" : ""}`}>
                  <button type="button" className="playground-conversation-open" aria-current={selected ? "true" : undefined} onClick={() => store.open(conversation.id)}>
                    <span className="playground-conversation-title">{titleOf(conversation)}</span>
                    <span className="playground-conversation-subtitle">{subtitle(conversation)}</span>
                  </button>
                  {busy && <Tooltip title={t("conversations.generating")}><LoadingOutlined className="playground-conversation-busy" aria-label={t("conversations.generating")} /></Tooltip>}
                  <Dropdown
                    trigger={["click"]}
                    menu={{
                      items: [
                        { key: "rename", icon: <EditOutlined />, label: t("conversations.rename") },
                        { key: "delete", icon: <DeleteOutlined />, label: t("conversations.delete"), danger: true },
                      ],
                      onClick: ({ key }) => key === "rename" ? setRenaming({ id: conversation.id, title: conversation.title }) : confirmDelete(conversation),
                    }}
                  >
                    <Button type="text" size="small" className="playground-conversation-more" icon={<MoreOutlined />} aria-label={t("conversations.more", { title: titleOf(conversation) })} />
                  </Dropdown>
                </div>
              );
            })}
          </section>
        ))}
      </div>
      <Typography.Text type="secondary" className="playground-conversations-foot">
        {persistent ? t("conversations.storedHere", { count: MAX_CONVERSATIONS }) : t("conversations.notStored")}
      </Typography.Text>
      {modalContext}
    </aside>
  );
}
