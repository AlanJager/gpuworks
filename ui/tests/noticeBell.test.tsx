import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n([]);
const zh = await createI18n([], "zh");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const NoticeBell = (await import("../src/components/NoticeBell.tsx")).default;
type NoticeList = import("../src/api.ts").NoticeList;
type NoticeItem = import("../src/api.ts").NoticeItem;

const n = locale.common.notices as Record<string, any>;
const original = { notices: apiModule.api.notices, readNotices: apiModule.api.readNotices };
afterEach(() => { cleanup(); Object.assign(apiModule.api, original); });
after(() => closeChannels());

function cve(read = false): NoticeItem {
  return {
    id: "cve-2026-0001", severity: "critical", kind: "security", read,
    title: { en: "Gateway CVE-2026-0001", zh: "网关漏洞 CVE-2026-0001" }, body: { en: "Upgrade to the latest build." },
    url: "https://gpuworks.ai/security/cve-2026-0001", published_at: "2026-10-21T00:00:00Z",
  };
}
const meetup: NoticeItem = { id: "meetup-1", severity: "info", kind: "event", read: false, title: { en: "Community meetup" }, published_at: "2026-10-20T00:00:00Z" };

// stub serves the notices and applies read marks the way the server does.
function stub(initial: NoticeItem[] | Error) {
  let items = initial instanceof Error ? [] : initial.map((x) => ({ ...x }));
  let failing: Error | null = initial instanceof Error ? initial : null;
  const reads: string[][] = [];
  let loads = 0;
  apiModule.api.notices = async (): Promise<NoticeList> => {
    loads++;
    if (failing) throw failing;
    return { notices: items.map((x) => ({ ...x })), unread: items.filter((x) => !x.read).length };
  };
  apiModule.api.readNotices = async (ids: string[]) => {
    reads.push(ids);
    items = items.map((x) => (ids.includes(x.id) ? { ...x, read: true } : x));
  };
  return { reads, loads: () => loads };
}

function renderBell(withI18n = i18n) {
  const user = userEvent.setup();
  render(
    <I18nextProvider i18n={withI18n}>
      <MemoryRouter initialEntries={["/dashboard"]}>
        <NoticeBell />
      </MemoryRouter>
    </I18nextProvider>,
  );
  return user;
}

const bell = () => screen.getByRole("button", { name: new RegExp(`^${n.title}`) });

test("the bell counts unread notices and lists the most urgent with its link", async () => {
  stub([cve(), meetup]);
  const user = renderBell();
  await waitFor(() => assert.equal(bell().getAttribute("aria-label"), `${n.title} (${n.unread.replace("{{count}}", "2")})`));
  await user.click(bell());
  const item = await screen.findByTestId("notice-cve-2026-0001");
  assert.ok(within(item).getByText("Gateway CVE-2026-0001"));
  assert.ok(within(item).getByText(n.severity.critical));
  assert.ok(within(item).getByText(n.kind.security));
  const link = within(item).getByText(n.readMore).closest("a")!;
  assert.equal(link.getAttribute("href"), "https://gpuworks.ai/security/cve-2026-0001");
  assert.equal(link.getAttribute("target"), "_blank");
  assert.ok((link.getAttribute("rel") ?? "").includes("noopener"));
  assert.ok(within(screen.getByTestId("notice-meetup-1")).getByText(n.kind.event));
});

test("marking one notice read, then all, updates the count", async () => {
  const api = stub([cve(), meetup]);
  const user = renderBell();
  await user.click(await waitFor(() => bell()));
  const item = await screen.findByTestId("notice-cve-2026-0001");
  await user.click(within(item).getByRole("button", { name: n.markRead }));
  await waitFor(() => assert.deepEqual(api.reads, [["cve-2026-0001"]]));
  await waitFor(() => assert.equal(bell().getAttribute("aria-label"), `${n.title} (${n.unread.replace("{{count}}", "1")})`));
  await user.click(screen.getByRole("button", { name: n.markAllRead }));
  await waitFor(() => assert.deepEqual(api.reads, [["cve-2026-0001"], ["meetup-1"]]));
  await waitFor(() => assert.equal(bell().getAttribute("aria-label"), n.title));
  assert.equal(screen.queryByRole("button", { name: n.markAllRead }), null);
});

test("an empty bell says so, and a failed load says it could not load", async () => {
  stub([]);
  const user = renderBell();
  await user.click(await waitFor(() => bell()));
  assert.ok(await screen.findByText(n.empty));
  cleanup();

  const failing = stub(new Error("network down"));
  const again = renderBell();
  await waitFor(() => assert.equal(failing.loads(), 1));
  // Opening the bell asks again; this answer fails too and the bell says so.
  await again.click(bell());
  await waitFor(() => assert.equal(failing.loads(), 2));
  assert.ok(await screen.findByText(n.loadFailed));
});

test("the publisher's text follows the console language, falling back to English", async () => {
  stub([cve(), { ...cve(), id: "event-1", kind: "event", severity: "info", title: { en: "Community meetup" }, body: undefined, url: undefined }]);
  const user = renderBell(zh.i18n);
  const title = zh.locale.common.notices.title as string;
  await user.click(await waitFor(() => screen.getByRole("button", { name: new RegExp(`^${title}`) })));
  assert.ok(await screen.findByText("网关漏洞 CVE-2026-0001"));
  assert.ok(screen.getByText("Community meetup"));
  assert.ok(screen.getByText("Upgrade to the latest build."));
});
