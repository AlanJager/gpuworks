import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
const { render, screen, fireEvent, waitFor, cleanup } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { SideDrawer } = await import("../src/ui-kit/SideDrawer.tsx");
const { NumberInput } = await import("../src/ui-kit/NumberInput.tsx");
const { FileUpload } = await import("../src/ui-kit/FileUpload.tsx");
const { ConfirmPopover } = await import("../src/ui-kit/ConfirmPopover.tsx");
const { Spinner } = await import("../src/ui-kit/Spinner.tsx");
const { Progress } = await import("../src/ui-kit/Progress.tsx");
const { Segmented } = await import("../src/ui-kit/Segmented.tsx");
const { Timeline } = await import("../src/ui-kit/Timeline.tsx");
const { Result } = await import("../src/ui-kit/Result.tsx");
const { i18n } = await createI18n([]);
const mount = (children: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{children}</I18nextProvider>);
afterEach(cleanup);

test("drawer dismisses with Escape and returns focus to its opener", async () => {
  function Example() {
    const [open, setOpen] = useState(false);
    return <><button onClick={() => setOpen(true)}>Inspect</button><SideDrawer open={open} onClose={() => setOpen(false)} title="Resource"><button>Action</button></SideDrawer></>;
  }
  mount(<Example />);
  const user = userEvent.setup();
  const opener = screen.getByRole("button", { name: "Inspect" });
  await user.click(opener);
  await screen.findByRole("dialog", { name: "Resource" });
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  await waitFor(() => assert.equal(document.activeElement, opener));
});

test("number input retains null and applies supplied bounds and precision on blur", () => {
  function Example() {
    const [value, setValue] = useState<number | null>(4);
    return <NumberInput aria-label="Hours" value={value} onChange={setValue} min={1} max={8} precision={0} />;
  }
  mount(<Example />);
  const input = screen.getByRole("spinbutton") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "" } });
  fireEvent.blur(input);
  assert.equal(input.value, "");
  fireEvent.change(input, { target: { value: "12" } });
  assert.equal(input.value, "12");
  fireEvent.blur(input);
  assert.equal(input.value, "8");
  fireEvent.change(input, { target: { value: "2.7" } });
  fireEvent.blur(input);
  assert.equal(input.value, "3");
});

test("file upload preserves selection on cancellation and allows removal and reselection", async () => {
  function Example() {
    const [file, setFile] = useState<File | null>(null);
    return <FileUpload aria-label="Requests" value={file} onChange={setFile} accept=".jsonl" />;
  }
  mount(<Example />);
  const input = screen.getByLabelText("Requests");
  const file = new window.File(["{}"], "requests.jsonl");
  fireEvent.change(input, { target: { files: [file] } });
  assert.ok(screen.getByText(file.name));
  assert.ok(screen.getByRole("button", { name: "Choose another" }), "the picker button says a file is chosen, in the console's language");
  fireEvent.change(input, { target: { files: [] } });
  assert.ok(screen.getByText(file.name));
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  assert.equal(screen.queryByText(file.name), null);
  assert.ok(screen.getByText("No file chosen"));
  fireEvent.change(input, { target: { files: [file] } });
  assert.ok(screen.getByText(file.name));
});

test("confirmation keeps failure visible and retries before closing", async () => {
  let attempts = 0;
  mount(<ConfirmPopover trigger={<button>Delete</button>} title="Delete resource?" confirmText="Proceed" onConfirm={async () => { if (++attempts === 1) throw new Error("Offline"); }} />);
  const user = userEvent.setup();
  await user.click(screen.getByRole("button", { name: "Delete" }));
  await user.click(await screen.findByRole("button", { name: "Proceed" }));
  assert.equal((await screen.findByRole("alert")).textContent, "Offline");
  await user.click(screen.getByRole("button", { name: "Proceed" }));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: "Proceed" }), null));
  assert.equal(attempts, 2);
});

test("spinner announces its status", () => {
  mount(<Spinner label="Loading records" />);
  assert.equal(screen.getByRole("status").textContent, "Loading records");
});
test("progress exposes a bounded determinate value", () => {
  mount(<Progress label="Completed requests" value={120} />);
  assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), "100");
});
test("segmented view switches with keyboard without selecting disabled choices", async () => {
  function Example() {
    const [value, setValue] = useState("a");
    return <Segmented label="Period" value={value} onChange={setValue} options={[{ value: "a", label: "Day" }, { value: "b", label: "Week", disabled: true }, { value: "c", label: "Month" }]} />;
  }
  mount(<Example />);
  screen.getByRole("tab", { name: "Day" }).focus();
  await userEvent.setup().keyboard("{ArrowRight}");
  assert.equal(screen.getByRole("tab", { name: "Week" }).getAttribute("aria-selected"), "false");
  await userEvent.setup().keyboard("{ArrowRight}");
  await waitFor(() => assert.equal(screen.getByRole("tab", { name: "Month" }).getAttribute("aria-selected"), "true"));
});
test("timeline retains ordered events and timestamps", () => {
  mount(<Timeline label="History" items={[{ key: "a", title: "Created", time: "2026-11-06" }, { key: "b", title: "Completed", description: "All requests processed" }]} />);
  assert.deepEqual(screen.getAllByRole("listitem").map((item) => item.textContent), ["Created2026-11-06", "CompletedAll requests processed"]);
});
test("result keeps its explanatory text and recovery action", () => {
  let retried = false;
  mount(<Result title="Unavailable" description="Retry later" actions={<button onClick={() => { retried = true; }}>Retry</button>} />);
  assert.ok(screen.getByText("Retry later"));
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  assert.equal(retried, true);
});
