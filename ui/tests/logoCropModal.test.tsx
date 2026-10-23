import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["settings"]);
const { act, cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { FULL_CROP, LogoError, squareCrop } = await import("../src/brandingLogo.ts");
const { default: LogoCropModal } = await import("../src/pages/settings/LogoCropModal.tsx");
const crop = locale.settings.branding.crop as Record<string, string>;
type LogoCrop = import("../src/brandingLogo.ts").LogoCrop;

// jsdom has no object URLs; the dialog only needs a stable string per file.
const created: string[] = [];
const revoked: string[] = [];
Object.assign(dom.window.URL, {
  createObjectURL: () => { const url = `blob:test/${created.length}`; created.push(url); return url; },
  revokeObjectURL: (url: string) => { revoked.push(url); },
});
Object.assign(URL, { createObjectURL: dom.window.URL.createObjectURL, revokeObjectURL: dom.window.URL.revokeObjectURL });

afterEach(() => cleanup());
after(() => closeChannels());

const near = (got: LogoCrop, want: LogoCrop) => {
  for (const key of ["x", "y", "width", "height"] as const) {
    assert.ok(Math.abs(got[key] - want[key]) < 1e-9, `${key}: got ${got[key]}, want ${want[key]}`);
  }
};

function setup({ prepare }: { prepare?: (file: File, crop: LogoCrop) => Promise<string> } = {}) {
  const calls: LogoCrop[] = [];
  const confirmed: string[] = [];
  let cancelled = 0;
  const file = new File([new Uint8Array(10)], "wide.png", { type: "image/png" });
  const user = userEvent.setup();
  const view = render(
    <I18nextProvider i18n={i18n}>
      <LogoCropModal
        file={file}
        onCancel={() => { cancelled++; }}
        onConfirm={(logo) => confirmed.push(logo)}
        prepare={prepare ?? (async (_file, c) => { calls.push(c); return "data:image/png;base64,AA=="; })}
      />
    </I18nextProvider>,
  );
  // The picture "loads" as a 400×100 image.
  const load = () => {
    const image = screen.getByAltText(crop.image) as HTMLImageElement;
    Object.defineProperty(image, "naturalWidth", { configurable: true, value: 400 });
    Object.defineProperty(image, "naturalHeight", { configurable: true, value: 100 });
    fireEvent.load(image);
  };
  return { user, view, calls, confirmed, cancelled: () => cancelled, load };
}
// After a save attempt antd keeps the spinner's "loading" label in the name
// while its exit animation runs, which never finishes under jsdom.
const ok = () => screen.getByRole("button", { name: new RegExp(`${crop.confirm}$`) }) as HTMLButtonElement;
const squareBox = () => screen.getByRole("checkbox", { name: crop.square }) as HTMLInputElement;

test("the selection starts as the whole image and confirming hands over the result", async () => {
  const s = setup();
  assert.ok(screen.getByText(crop.title));
  assert.equal(ok().disabled, true, "nothing to crop before the image loads");
  assert.equal(squareBox().disabled, true);
  s.load();
  await waitFor(() => assert.equal(ok().disabled, false));
  assert.equal(squareBox().checked, false);
  await s.user.click(ok());
  await waitFor(() => assert.deepEqual(s.confirmed, ["data:image/png;base64,AA=="]));
  near(s.calls[0], FULL_CROP);
});

test("square snaps to the centered square, reset keeps the square, unchecking frees it", async () => {
  const s = setup();
  s.load();
  await waitFor(() => assert.equal(squareBox().disabled, false));
  await s.user.click(squareBox());
  assert.equal(squareBox().checked, true);
  const preview = screen.getByTestId("logo-crop-preview").firstElementChild as HTMLElement;
  assert.equal(preview.style.width, "24px");
  assert.equal(preview.style.height, "24px", "a square stops at the top bar's 24 px height");
  await s.user.click(ok());
  await waitFor(() => assert.equal(s.calls.length, 1));
  near(s.calls[0], squareCrop(400, 100));

  await s.user.click(screen.getByRole("button", { name: crop.reset }));
  await s.user.click(ok());
  await waitFor(() => assert.equal(s.calls.length, 2));
  near(s.calls[1], squareCrop(400, 100));

  // The checkbox works from the keyboard too.
  squareBox().focus();
  await s.user.keyboard(" ");
  assert.equal(squareBox().checked, false);
  const wide = screen.getByTestId("logo-crop-preview").firstElementChild as HTMLElement;
  assert.equal(wide.style.width, "96px");
  assert.equal(wide.style.height, "24px", "the whole 4:1 image keeps its shape, stopped by the height");
  await s.user.click(ok());
  await waitFor(() => assert.equal(s.calls.length, 3));
  near(s.calls[2], FULL_CROP);
});

test("cancel closes without a result, and a failed compression keeps the dialog open", async () => {
  const s = setup({ prepare: async () => { throw new LogoError("tooLarge"); } });
  s.load();
  await waitFor(() => assert.equal(ok().disabled, false));
  await s.user.click(ok());
  assert.ok(await screen.findByText(locale.settings.branding.logoError.tooLarge.replace("{{max}}", "128")));
  assert.deepEqual(s.confirmed, []);
  assert.equal(ok().disabled, false, "the administrator can adjust and try again");
  await s.user.click(screen.getByRole("button", { name: locale.settings.common.cancel }));
  assert.equal(s.cancelled(), 1);
});

test("a click on the mask does not close the dialog", async () => {
  const s = setup();
  const mask = document.querySelector(".ant-modal-wrap") as HTMLElement;
  fireEvent.mouseDown(mask);
  fireEvent.mouseUp(mask);
  fireEvent.click(mask);
  assert.equal(s.cancelled(), 0);
  assert.ok(screen.getByText(crop.title));
});

test("closing the dialog releases the image's object URL", async () => {
  const before = revoked.length;
  const s = setup();
  const url = created[created.length - 1];
  s.view.unmount();
  await act(async () => {});
  assert.ok(revoked.slice(before).includes(url));
});
