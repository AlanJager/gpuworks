// The fallback collector of the user experience improvement program
// (docs/reference/usage-report.md, ADR 0054): a Cloudflare Worker at
// https://usage.gpuworks.ai/v1/report that takes the report a server could not
// deliver to the SLS logstore and keeps it in the R2 bucket bound as REPORTS,
// one object per installation and day (a second delivery of the same day
// replaces the first), with the public address it came from and that
// address's country and region in the object's metadata, as the logstore
// records the client address of what it takes. The publisher imports the
// objects into the logstore.
//
// Deploy: wrangler deploy (wrangler.toml beside this file).

const maxBody = 256 << 10; // gzip; a report is at most 128 KiB before compression
const installID = /^[0-9a-f]{16}$/;
const day = /^\d{4}-\d{2}-\d{2}$/;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname !== "/v1/report") return new Response("not found", { status: 404 });
    if (request.method !== "POST") return new Response("method not allowed", { status: 405, headers: { Allow: "POST" } });
    const declared = Number(request.headers.get("Content-Length") || "0");
    if (declared > maxBody) return new Response("too large", { status: 413 });
    const raw = new Uint8Array(await request.arrayBuffer());
    if (raw.byteLength > maxBody) return new Response("too large", { status: 413 });
    let text;
    try {
      text = await decode(raw);
    } catch {
      return new Response("body is not gzip", { status: 400 });
    }
    if (text.length > 2 * 128 << 10) return new Response("too large", { status: 413 });
    let report;
    try {
      report = JSON.parse(text);
    } catch {
      return new Response("body is not JSON", { status: 400 });
    }
    if (!Number.isInteger(report.schema) || report.schema < 1 || !installID.test(report.install_id ?? "") || !day.test(report.day ?? "")) {
      return new Response("not a usage report", { status: 400 });
    }
    await env.REPORTS.put(`reports/${report.day}/${report.install_id}.json`, text, {
      httpMetadata: { contentType: "application/json" },
      customMetadata: {
        schema: String(report.schema),
        received: new Date().toISOString(),
        client_ip: request.headers.get("CF-Connecting-IP") ?? "",
        country: request.cf?.country ?? "",
        region: request.cf?.region ?? "",
      },
    });
    return new Response(null, { status: 204 });
  },
};

// decode gunzips the body; Cloudflare may already have decompressed it, in
// which case it is taken as it is.
async function decode(raw) {
  if (raw[0] === 0x1f && raw[1] === 0x8b) {
    const stream = new Blob([raw]).stream().pipeThrough(new DecompressionStream("gzip"));
    return await new Response(stream).text();
  }
  return new TextDecoder().decode(raw);
}
