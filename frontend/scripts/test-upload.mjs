import { test, afterEach, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createRequire } from "node:module";
import ts from "typescript";

// Compile only the transport and its API dependency; no browser or real account
// is involved. Each test drives server responses independently of byte progress.
const temp = mkdtempSync(join(tmpdir(), "owlet-upload-test-"));
for (const name of ["api", "upload"]) {
  const source = readFileSync(new URL(`../src/${name}.ts`, import.meta.url), "utf8");
  writeFileSync(join(temp, `${name}.js`), ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
  }).outputText);
}
const { uploadChunk, uploadProgress } = createRequire(import.meta.url)(join(temp, "upload.js"));
const originalXHR = globalThis.XMLHttpRequest;
const originalFetch = globalThis.fetch;
class XHR {
  static requests = [];
  upload = {};
  headers = {};
  status = 0;
  responseText = "";
  open(method, url) { this.method = method; this.url = url; }
  setRequestHeader(key, value) { this.headers[key] = value; }
  send(body) { this.body = body; XHR.requests.push(this); }
  abort() { this.aborted = true; this.onabort?.(); }
  respond(status, body = {}) { this.status = status; this.responseText = JSON.stringify(body); this.onload(); }
}
globalThis.XMLHttpRequest = XHR;
afterEach(() => { XHR.requests = []; globalThis.fetch = originalFetch; });
after(() => { globalThis.XMLHttpRequest = originalXHR; rmSync(temp, { recursive: true, force: true }); });
const part = new Blob([new Uint8Array(100)]);

test("resume includes confirmed bytes and partial final chunk", () => {
  assert.equal(uploadProgress(13, 5, new Set([0, 1]), new Map()), 68);
  assert.equal(uploadProgress(13, 5, new Set([0, 1, 2]), new Map()), 85);
  assert.equal(uploadProgress(20, 5, new Set([0]), new Map([[0, 5], [1, 2]])), 39);
});
test("byte progress does not acknowledge a chunk before the server", async () => {
  let bytes = 0, resolved = false;
  const request = uploadChunk("/uploads/id/chunks/0", part, "md5", new AbortController().signal, n => bytes = n).then(() => resolved = true);
  const xhr = XHR.requests[0];
  assert.equal(xhr.method, "PUT");
  assert.equal(xhr.headers["X-Chunk-MD5"], "md5");
  xhr.upload.onprogress({ loaded: 100 });
  await Promise.resolve();
  assert.equal(bytes, 100);
  assert.equal(resolved, false);
  xhr.respond(200);
  await request;
  assert.equal(resolved, true);
});
test("network errors and timeouts reject with actionable codes", async () => {
  for (const [event, code] of [["onerror", "upload_network_error"], ["ontimeout", "upload_timeout"]]) {
    const request = uploadChunk("/uploads/id/chunks/0", part, "md5", new AbortController().signal, () => {});
    XHR.requests.at(-1)[event]();
    await assert.rejects(request, new RegExp(code));
  }
});
test("failure cancellation interrupts all in-flight peers", async () => {
  const controller = new AbortController();
  const requests = [0, 1, 2].map(i => uploadChunk(`/uploads/id/chunks/${i}`, part, "md5", controller.signal, () => {}));
  const settled = Promise.allSettled(requests);
  controller.abort();
  assert.ok(XHR.requests.every(xhr => xhr.aborted));
  assert.ok((await settled).every(r => r.status === "rejected"));
});
test("a stalled connection is aborted after the idle deadline", async () => {
  const originalTimer = globalThis.setTimeout;
  let expire;
  globalThis.setTimeout = (fn, ms, ...args) => {
    if (ms === 120000) expire = fn;
    return originalTimer(fn, ms, ...args);
  };
  let request;
  try {
    request = uploadChunk("/uploads/id/chunks/0", part, "md5", new AbortController().signal, () => {});
  } finally { globalThis.setTimeout = originalTimer; }
  assert.equal(typeof expire, "function");
  expire();
  await assert.rejects(request, /upload_timeout/);
  assert.equal(XHR.requests[0].aborted, true);
});
test("401 refreshes once, resets byte progress, then sends the same chunk", async () => {
  let refreshes = 0;
  globalThis.fetch = async () => { refreshes++; return { ok: true }; };
  const progress = [];
  const request = uploadChunk("/uploads/id/chunks/0", part, "md5", new AbortController().signal, n => progress.push(n));
  XHR.requests[0].respond(401, { error: "invalid_session" });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(refreshes, 1);
  assert.equal(XHR.requests.length, 2);
  assert.deepEqual(progress, [0]);
  assert.equal(XHR.requests[1].body, part);
  XHR.requests[1].respond(200);
  await request;
});
test("a rejected refreshed session stops instead of refreshing indefinitely", async () => {
  globalThis.fetch = async () => ({ ok: true });
  const request = uploadChunk("/uploads/id/chunks/0", part, "md5", new AbortController().signal, () => {});
  XHR.requests[0].respond(401);
  await new Promise(resolve => setImmediate(resolve));
  XHR.requests[1].respond(401, { error: "invalid_session" });
  await assert.rejects(request, /invalid_session/);
  assert.equal(XHR.requests.length, 2);
});
