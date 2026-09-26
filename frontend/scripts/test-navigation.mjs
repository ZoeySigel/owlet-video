import { test, afterEach, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createRequire } from "node:module";
import ts from "typescript";

const temp = mkdtempSync(join(tmpdir(), "owlet-navigation-test-"));
const source = readFileSync(new URL("../src/api.ts", import.meta.url), "utf8");
writeFileSync(join(temp, "api.js"), ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText);
const { api, cachedPublic, clearPublicCache, readResource, rememberVideo } = createRequire(import.meta.url)(join(temp, "api.js"));
const originalFetch = globalThis.fetch;
const originalNow = Date.now;
const response = value => ({ ok: true, status: 200, json: async () => value });
afterEach(() => { globalThis.fetch = originalFetch; Date.now = originalNow; clearPublicCache(); });
after(() => rmSync(temp, { recursive: true, force: true }));

test("concurrent public reads and immediate revisits require one request", async () => {
  let requests = 0, release;
  globalThis.fetch = () => { requests++; return new Promise(resolve => release = resolve); };
  const one = readResource("/videos?sort=latest"), two = readResource("/videos?sort=latest");
  assert.equal(requests, 1);
  release(response({ items: [{ id: 1 }] }));
  await Promise.all([one, two]);
  assert.deepEqual(await readResource("/videos?sort=latest"), { items: [{ id: 1 }] });
  assert.equal(requests, 1);
});
test("public cache expires and explicit reload bypasses it", async () => {
  let now = 10000, requests = 0;
  Date.now = () => now;
  globalThis.fetch = async () => response({ id: ++requests });
  await readResource("/videos/1");
  now += 15001;
  assert.equal(cachedPublic("/videos/1"), null);
  await readResource("/videos/1");
  await readResource("/videos/1", true);
  assert.equal(requests, 3);
});
test("private resources and following feed are never cached", async () => {
  let requests = 0;
  globalThis.fetch = async () => { requests++; return response({}); };
  for (const path of ["/messages", "/notifications", "/auth/me", "/videos/1/like", "/videos?sort=following", "/me/likes"]) {
    await readResource(path); await readResource(path);
    assert.equal(cachedPublic(path), null);
  }
  assert.equal(requests, 12);
});
test("mutations invalidate cached content and fence older in-flight reads", async () => {
  let release;
  globalThis.fetch = (path, options) => options.method === "PUT"
    ? Promise.resolve(response({ liked: true }))
    : new Promise(resolve => release = resolve);
  rememberVideo({ id: 1, title: "before" });
  const old = readResource("/videos/2");
  await api("/videos/1/like", { method: "PUT" });
  assert.equal(cachedPublic("/videos/1"), null);
  release(response({ id: 2 }));
  await old;
  assert.equal(cachedPublic("/videos/2"), null);
});
test("list video data opens detail without another metadata round trip", async () => {
  globalThis.fetch = async () => { throw new Error("unexpected network request"); };
  const video = { id: 7, title: "existing list data", playUrl: "/media/test.mp4" };
  rememberVideo(video);
  assert.deepEqual(await readResource("/videos/7"), video);
});
test("failed reads are retryable and the cache has a fixed entry bound", async () => {
  globalThis.fetch = async () => { throw new Error("offline"); };
  await assert.rejects(readResource("/videos/1"), /offline/);
  globalThis.fetch = async () => response({ id: 1 });
  await readResource("/videos/1");
  for (let id = 2; id <= 65; id++) rememberVideo({ id });
  assert.equal(cachedPublic("/videos/1"), null);
  assert.deepEqual(cachedPublic("/videos/65"), { id: 65 });
});
