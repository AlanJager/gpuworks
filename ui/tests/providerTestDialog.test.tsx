// The connection-test dialog (ui/src/components/ProviderTestDialog.tsx): the
// streamed checks as they arrive, the summary when the run settles, and what
// a failure, a skip, a test that could not run and a broken stream each leave
// on the page; closing it, from the keyboard too, ends the request. The run
// is injected, so no network is involved; the request itself is covered by
// tests/providerTestStream.test.ts and tests/providerConfigModalTest.test.tsx.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { freshPopupFramesPerTest, framesSettled } from "./helpers/platformDialog.ts";

const en = await createI18n(["providers"], "en");
const zh = await createI18n(["providers"], "zh");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { ConnectionTestInterrupted } = await import("../src/api.ts");
const { default: ProviderTestDialog } = await import("../src/components/ProviderTestDialog.tsx");

type Check = import("../src/api.ts").ConnectionCheckEvent;
type Summary = import("../src/api.ts").ConnectionTestSummary;
type Run = (onCheck: (c: Check) => void, signal: AbortSignal) => Promise<Summary>;

const strings = en.locale.providers.test;
const apiError = en.locale.common.apiError;

afterEach(() => cleanup());
await freshPopupFramesPerTest();

function renderDialog(run: Run, onClose: () => void = () => {}, language: "en" | "zh" = "en") {
  return render(<I18nextProvider i18n={(language === "zh" ? zh : en).i18n}><ProviderTestDialog open run={run} onClose={onClose} /></I18nextProvider>);
}
/** The row of a check, by its name as shown. */
const rowOf = (name: string) => screen.getByText(name).closest("li") as HTMLElement;
/** A row's lines as read: name, status or duration, then what it says. */
const linesOf = (name: string) => [...rowOf(name).querySelectorAll("span")].filter((span) => span.children.length === 0).map((span) => span.textContent);
const statusOf = (name: string) => rowOf(name).querySelector("[data-status]")?.getAttribute("data-status");
/** The summary notice: an alert when the run failed, a status when it passed. */
const summary = () => {
  const notice = document.querySelector('[data-slot="alert"]') as HTMLElement;
  assert.ok(notice, "the run has its summary");
  return notice;
};
/** A run the test settles by hand, with the signal the dialog gave it. */
function heldRun() {
  const held: { onCheck?: (c: Check) => void; signal?: AbortSignal; resolve?: (s: Summary) => void; reject?: (e: unknown) => void; started: number } = { started: 0 };
  const run: Run = (onCheck, signal) => {
    held.started += 1;
    held.onCheck = onCheck;
    held.signal = signal;
    return new Promise<Summary>((resolve, reject) => { held.resolve = resolve; held.reject = reject; });
  };
  return { held, run };
}

test("a pending run shows the spinner and no checks, and claims no outcome", async () => {
  // The promise never settles: the dialog keeps waiting instead of showing a summary.
  renderDialog(() => new Promise<Summary>(() => {}));
  assert.equal(screen.getByRole("status").textContent, strings.running);
  assert.equal(document.querySelectorAll("li").length, 0, "no checks have arrived yet");
  assert.equal(screen.queryByText(strings.ok), null, "a pending run never claims success");
  assert.equal(screen.queryByText(strings.failed), null, "a pending run never claims failure");
});

test("checks appear as they stream in, in the server's order, while the run is still going", async () => {
  const { held, run } = heldRun();
  renderDialog(run);
  await waitFor(() => assert.equal(held.started, 1));
  held.onCheck?.({ seq: 2, name: "credentials_shape", status: "ok", duration_ms: 0 });
  held.onCheck?.({ seq: 1, name: "endpoint_format", status: "ok", duration_ms: 1 });
  await screen.findByText(strings.check.credentials_shape);
  assert.deepEqual([...document.querySelectorAll("li")].map((li) => li.querySelector("span span")?.textContent), [strings.check.endpoint_format, strings.check.credentials_shape]);
  assert.equal(screen.getByRole("status").textContent, strings.running, "still running");
  assert.equal(document.querySelector('[data-slot="alert"]'), null, "no outcome yet");
  held.resolve?.({ ok: true });
  await screen.findByText(strings.ok);
});

test("streamed checks render with translated names and durations, and the run ends with the success summary", async () => {
  const run: Run = (onCheck) => {
    onCheck({ seq: 1, name: "endpoint_format", status: "ok", duration_ms: 1 });
    onCheck({ seq: 2, name: "tcp_connect", status: "ok", duration_ms: 12 });
    onCheck({ seq: 3, name: "auth_request", status: "ok", duration_ms: 240 });
    return Promise.resolve({ ok: true });
  };
  renderDialog(run);
  await screen.findByText(strings.ok);
  assert.deepEqual(linesOf(strings.check.endpoint_format), [strings.check.endpoint_format, strings.status.ok, "1ms"]);
  assert.deepEqual(linesOf(strings.check.tcp_connect), [strings.check.tcp_connect, strings.status.ok, "12ms"]);
  assert.deepEqual(linesOf(strings.check.auth_request), [strings.check.auth_request, strings.status.ok, "240ms"]);
  assert.equal(statusOf(strings.check.auth_request), "ok");
  assert.equal(screen.queryByText(strings.running), null, "the spinner is gone");
  assert.equal(screen.queryByText(strings.failed), null);
  assert.equal(summary().textContent, strings.ok, "a passed run needs no more words");
});

test("a failed check shows its translated error and what to do, and the run ends with the failure summary", async () => {
  const run: Run = (onCheck) => {
    onCheck({ seq: 1, name: "endpoint_format", status: "ok", duration_ms: 1 });
    onCheck({ seq: 2, name: "tcp_connect", status: "failed", duration_ms: 5003, code: "PROVIDER_TCP_REFUSED", detail: { port: 443 }, hint: "tcp_refused" });
    return Promise.resolve({ ok: false, failed: ["tcp_connect"] });
  };
  renderDialog(run);
  await screen.findByText(strings.failed);
  assert.ok(screen.getByText(strings.check.endpoint_format), "the earlier ok check stays visible");
  assert.deepEqual(linesOf(strings.check.tcp_connect), [strings.check.tcp_connect, strings.status.failed, "5003ms", apiError.PROVIDER_TCP_REFUSED, strings.hint.tcp_refused]);
  assert.equal(statusOf(strings.check.tcp_connect), "failed");
  assert.equal(screen.queryByText(strings.ok), null, "a failed run never claims success");
  assert.equal(summary().textContent, strings.failed, "the failed rows say why; the summary does not repeat them");
  assert.equal(summary().getAttribute("role"), "alert", "a failure is announced");
});

test("rejected credentials read as the platform's refusal, with what to check", async () => {
  const run: Run = (onCheck) => {
    onCheck({ seq: 8, name: "auth_request", status: "failed", duration_ms: 310, code: "PROVIDER_AUTH_FAILED", hint: "auth" });
    return Promise.resolve({ ok: false, failed: ["auth_request"] });
  };
  renderDialog(run);
  await screen.findByText(strings.failed);
  assert.deepEqual(linesOf(strings.check.auth_request), [strings.check.auth_request, strings.status.failed, "310ms", apiError.PROVIDER_AUTH_FAILED, strings.hint.auth]);
});

test("a proxy that cannot be reached fails the proxy check, with what to check", async () => {
  const run: Run = (onCheck) => {
    onCheck({ seq: 6, name: "proxy_detect", status: "failed", duration_ms: 3001, code: "PROVIDER_PROXY_FAILED", detail: { proxy: "env", variables: ["HTTPS_PROXY"], used: true }, hint: "proxy" });
    return Promise.resolve({ ok: false, failed: ["proxy_detect"] });
  };
  renderDialog(run);
  await screen.findByText(strings.failed);
  assert.deepEqual(linesOf(strings.check.proxy_detect), [strings.check.proxy_detect, strings.status.failed, "3001ms", apiError.PROVIDER_PROXY_FAILED, strings.hint.proxy]);
});

test("a check skipped because an earlier one failed says so, in words and not as a failure", async () => {
  const run: Run = (onCheck) => {
    onCheck({ seq: 1, name: "endpoint_format", status: "failed", duration_ms: 0, code: "PROVIDER_ENDPOINT_INVALID", hint: "endpoint_empty" });
    onCheck({ seq: 2, name: "dns_resolve", status: "skipped", duration_ms: 0, hint: "depends_failed", detail: { depends: "endpoint_format" } });
    return Promise.resolve({ ok: false, failed: ["endpoint_format"] });
  };
  renderDialog(run);
  await screen.findByText(strings.failed);
  assert.deepEqual(linesOf(strings.check.dns_resolve), [strings.check.dns_resolve, strings.status.skipped, strings.hint.depends_failed], "a check that did not run shows no duration");
  assert.equal(statusOf(strings.check.dns_resolve), "skipped");
  assert.equal(rowOf(strings.check.dns_resolve).querySelector(".text-fg-danger"), null, "nothing in a skipped row is in the failure colour");
});

for (const language of ["en", "zh"] as const) {
  test(`in ${language} an account type with no single endpoint (Alibaba Cloud) has its endpoint and network checks skipped as not applicable, and still passes`, async () => {
    const copy = (language === "zh" ? zh : en).locale.providers.test;
    const skipped = ["endpoint_format", "dns_resolve", "tcp_connect", "tls_handshake", "clock_skew"];
    const run: Run = (onCheck) => {
      skipped.slice(0, 1).forEach((name, index) => onCheck({ seq: index + 1, name, status: "skipped", duration_ms: 0, hint: "not_applicable" }));
      onCheck({ seq: 2, name: "credentials_shape", status: "ok", duration_ms: 0 });
      skipped.slice(1).forEach((name, index) => onCheck({ seq: index + 3, name, status: "skipped", duration_ms: 0, hint: "not_applicable" }));
      onCheck({ seq: 7, name: "proxy_detect", status: "ok", duration_ms: 0, detail: { proxy: "none" } });
      onCheck({ seq: 8, name: "auth_request", status: "ok", duration_ms: 420 });
      return Promise.resolve({ ok: true });
    };
    renderDialog(run, () => {}, language);
    await screen.findByText(copy.ok);
    for (const name of skipped) {
      assert.deepEqual(linesOf(copy.check[name]), [copy.check[name], copy.status.skipped, copy.hint.not_applicable]);
      assert.equal(statusOf(copy.check[name]), "skipped");
      assert.equal(rowOf(copy.check[name]).querySelector(".text-fg-danger"), null);
    }
    assert.deepEqual(linesOf(copy.check.auth_request), [copy.check.auth_request, copy.status.ok, "420ms"]);
    assert.deepEqual(linesOf(copy.check.proxy_detect), [copy.check.proxy_detect, copy.status.ok, "0ms"], "no proxy, nothing to say");
    assert.equal(summary().textContent, copy.ok);
    assert.equal(screen.queryByText(copy.failed), null, "skipped checks do not fail the run");
    assert.equal(document.querySelector(".text-fg-danger"), null);
  });
}

test("behind a proxy the proxy check names the variables that set it, and the TLS check is skipped as going through it", async () => {
  const run: Run = (onCheck) => {
    onCheck({ seq: 5, name: "tls_handshake", status: "skipped", duration_ms: 0, hint: "via_proxy" });
    onCheck({ seq: 6, name: "proxy_detect", status: "ok", duration_ms: 2, detail: { proxy: "env", variables: ["HTTPS_PROXY", "NO_PROXY"], used: true } });
    return Promise.resolve({ ok: true });
  };
  renderDialog(run);
  await screen.findByText(strings.ok);
  assert.deepEqual(linesOf(strings.check.tls_handshake), [strings.check.tls_handshake, strings.status.skipped, strings.hint.via_proxy]);
  assert.deepEqual(linesOf(strings.check.proxy_detect), [strings.check.proxy_detect, strings.status.ok, "2ms", "Through the proxy set in HTTPS_PROXY, NO_PROXY"]);
});

test("a proxy that is set but not used for this address is said to be unused", async () => {
  const run: Run = (onCheck) => {
    onCheck({ seq: 6, name: "proxy_detect", status: "ok", duration_ms: 0, detail: { proxy: "env", variables: ["HTTPS_PROXY", "NO_PROXY"], used: false } });
    return Promise.resolve({ ok: true });
  };
  renderDialog(run);
  await screen.findByText(strings.ok);
  assert.deepEqual(linesOf(strings.check.proxy_detect), [strings.check.proxy_detect, strings.status.ok, "0ms", "Proxy set in HTTPS_PROXY, NO_PROXY, not used for this address"]);
});

for (const language of ["en", "zh"] as const) {
  test(`in ${language} a test that could not run at all shows no checks and says so from the summary's code`, async () => {
    const locale = (language === "zh" ? zh : en).locale;
    renderDialog(() => Promise.resolve({ ok: false, code: "PROVIDER_TEST_UNAVAILABLE" }), () => {}, language);
    await screen.findByText(locale.providers.test.failed);
    assert.equal(document.querySelectorAll("li").length, 0, "no check ran");
    assert.ok(within(summary()).getByText(locale.common.apiError.PROVIDER_TEST_UNAVAILABLE));
    assert.equal(screen.queryByText(locale.providers.test.running), null);
  });
}

test("a summary code this console has no words for falls back to the plain failure, never the raw code", async () => {
  renderDialog(() => Promise.resolve({ ok: false, code: "PROVIDER_FROM_A_NEWER_SERVER" }));
  await screen.findByText(strings.failed);
  assert.equal(summary().textContent, strings.failed);
  assert.doesNotMatch(document.body.textContent ?? "", /PROVIDER_FROM_A_NEWER_SERVER/);
});

test("a stream that breaks mid-way keeps the checks that arrived and ends as an interrupted test, not a hanging one", async () => {
  const { held, run } = heldRun();
  renderDialog(run);
  await waitFor(() => assert.equal(held.started, 1));
  held.onCheck?.({ seq: 1, name: "endpoint_format", status: "ok", duration_ms: 1 });
  held.onCheck?.({ seq: 2, name: "dns_resolve", status: "ok", duration_ms: 8 });
  await screen.findByText(strings.check.dns_resolve);
  held.reject?.(new ConnectionTestInterrupted());
  await screen.findByText(strings.failed);
  assert.ok(within(summary()).getByText(strings.interrupted), "what happened and what to do, in the reader's language");
  assert.ok(screen.getByText(strings.check.endpoint_format), "what had arrived stays");
  assert.equal(screen.queryByText(strings.running), null, "the spinner is gone");
  assert.doesNotMatch(document.body.textContent ?? "", /stream ended|\/api\/v1/, "no internal wording reaches the page");
});

test("a refused request shows the server's reason instead of hanging", async () => {
  // What api.ts rejects with for an error response: the product error, already in the reader's language.
  renderDialog(() => Promise.reject(new Error(apiError.PROVIDER_KIND_UNKNOWN)));
  await screen.findByText(strings.failed);
  assert.ok(within(summary()).getByText(apiError.PROVIDER_KIND_UNKNOWN));
  assert.equal(screen.queryByText(strings.running), null, "the spinner is gone");
});

test("Escape closes the dialog and ends a request that is still running; what arrives afterwards is dropped", async () => {
  const { held, run } = heldRun();
  let closed = 0;
  const view = renderDialog(run, () => { closed += 1; });
  await waitFor(() => assert.equal(held.started, 1));
  assert.equal(held.signal?.aborted, false);
  await framesSettled();
  const user = userEvent.setup({ document: dom.window.document });
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(closed, 1, "Escape triggers onClose"));
  // The owner closes it, as both owners do.
  view.rerender(<I18nextProvider i18n={en.i18n}><ProviderTestDialog open={false} run={run} onClose={() => {}} /></I18nextProvider>);
  await waitFor(() => assert.equal(held.signal?.aborted, true, "closing aborts the request"));
  held.onCheck?.({ seq: 1, name: "endpoint_format", status: "ok", duration_ms: 1 });
  held.reject?.(held.signal?.reason);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(screen.queryByText(strings.check.endpoint_format), null, "a late check is not shown");
  assert.equal(screen.queryByText(strings.failed), null, "an aborted test is not a failed one");
  assert.equal(held.started, 1);
});

test("the close button is reached and pressed from the keyboard", async () => {
  let closed = 0;
  renderDialog(() => Promise.resolve({ ok: true }), () => { closed += 1; });
  await screen.findByText(strings.ok);
  await framesSettled();
  const close = screen.getByRole("button", { name: en.locale.common.actions.close });
  const user = userEvent.setup({ document: dom.window.document });
  for (let presses = 0; presses < 3 && document.activeElement !== close; presses += 1) await user.tab();
  assert.ok(document.activeElement === close, "Tab reaches the close button");
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(closed, 1));
});

test("unmounting the dialog ends its request", async () => {
  const { held, run } = heldRun();
  const view = renderDialog(run);
  await waitFor(() => assert.equal(held.started, 1));
  view.unmount();
  assert.equal(held.signal?.aborted, true);
});

test("opening it again runs the test afresh, with none of the last run left on the page", async () => {
  let runs = 0;
  const run: Run = (onCheck) => {
    runs += 1;
    if (runs === 1) {
      onCheck({ seq: 1, name: "auth_request", status: "failed", duration_ms: 9, code: "PROVIDER_AUTH_FAILED", hint: "auth" });
      return Promise.resolve({ ok: false, failed: ["auth_request"] });
    }
    return new Promise<Summary>(() => {});
  };
  const view = renderDialog(run);
  await screen.findByText(strings.failed);
  const tree = (open: boolean) => <I18nextProvider i18n={en.i18n}><ProviderTestDialog open={open} run={run} onClose={() => {}} /></I18nextProvider>;
  view.rerender(tree(false));
  view.rerender(tree(true));
  await waitFor(() => assert.equal(runs, 2));
  await waitFor(() => assert.equal(screen.getByRole("status").textContent, strings.running));
  assert.equal(screen.queryByText(strings.failed), null);
  assert.equal(document.querySelectorAll("li").length, 0);
});
