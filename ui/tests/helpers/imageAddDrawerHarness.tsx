import assert from "node:assert/strict";
import { afterEach } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { dom } from "./dom.ts";
import { catalogRoutes, i18n, installFetch, zhImages, type Handler } from "./imagesHarness.ts";

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { message } = await import("antd");
const { toast, Toaster } = await import("../../src/components/ui/toast.tsx");
const { default: ImagesPage } = await import("../../src/pages/ImagesPage.tsx");

let restoreFetch = () => {};
afterEach(() => {
  cleanup();
  toast.dismiss();
  message.destroy();
  restoreFetch();
});

export function renderPage(...handlers: Handler[]) {
  const stub = installFetch(async (call) => {
    for (const handler of handlers) {
      const res = await handler(call);
      if (res) return res;
    }
    return catalogRoutes(call);
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/images"]}><ImagesPage /><Toaster duration={0} /></MemoryRouter></I18nextProvider>);
  return { calls: stub.calls, user: userEvent.setup({ document: dom.window.document }) };
}

export async function openDrawer(user: ReturnType<typeof userEvent.setup>, tab: "社区镜像" | "自定义镜像") {
  await user.click(screen.getByRole("tab", { name: tab }));
  await user.click(await screen.findByRole("button", { name: tab === "社区镜像" ? /添加社区镜像/ : /添加自定义镜像/ }));
  const title = zhImages.import.title[tab === "社区镜像" ? "community" : "custom"];
  return await waitFor(() => {
    const heading = [...document.querySelectorAll(".ant-drawer-title")].find((node) => node.textContent === title);
    assert.ok(heading, `drawer ${title}`);
    return heading.closest(".ant-drawer-content") as HTMLElement;
  });
}

export async function choose(field: HTMLElement, title: string) {
  fireEvent.mouseDown(field);
  const option = await waitFor(() => {
    const found = [...document.querySelectorAll(".ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option")]
      .find((node) => node.getAttribute("title") === title);
    assert.ok(found, `option ${title}`);
    return found as HTMLElement;
  });
  fireEvent.click(option);
}
