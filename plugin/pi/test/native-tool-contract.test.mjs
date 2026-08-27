import assert from "node:assert/strict";
import { mkdir, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

const ROOT = dirname(dirname(fileURLToPath(import.meta.url)));
const NODE_MODULES = join(ROOT, "node_modules");

async function installRuntimeStubs() {
  await mkdir(join(NODE_MODULES, "@earendil-works", "pi-tui"), { recursive: true });
  await writeFile(
    join(NODE_MODULES, "@earendil-works", "pi-tui", "package.json"),
    JSON.stringify({ type: "module", exports: "./index.js" }),
  );
  await writeFile(
    join(NODE_MODULES, "@earendil-works", "pi-tui", "index.js"),
    "export class Text { constructor(text) { this.text = text; } }\n",
  );

  await mkdir(join(NODE_MODULES, "typebox"), { recursive: true });
  await writeFile(
    join(NODE_MODULES, "typebox", "package.json"),
    JSON.stringify({ type: "module", exports: "./index.js" }),
  );
  await writeFile(
    join(NODE_MODULES, "typebox", "index.js"),
    `const schema = (kind) => (...args) => ({ kind, args });
export const Type = new Proxy({}, { get: (_target, prop) => schema(String(prop)) });
`,
  );
}

function deferred() {
  let resolve;
  const promise = new Promise((settle) => {
    resolve = settle;
  });
  return { promise, resolve };
}

async function loadPluginHarness(tag) {
  await installRuntimeStubs();
  const registeredTools = new Map();
  const eventHandlers = new Map();
  const pluginUrl = pathToFileURL(join(ROOT, "index.ts"));
  pluginUrl.search = `?${tag}=${Date.now()}`;
  const { default: registerEngram } = await import(pluginUrl.href);
  registerEngram({
    registerTool(tool) {
      registeredTools.set(tool.name, tool);
    },
    on(event, handler) {
      eventHandlers.set(event, handler);
    },
  });
  return { registeredTools, eventHandlers };
}

function runtimeContext(sessionId) {
  return {
    cwd: ROOT,
    sessionManager: { getSessionId: () => sessionId },
    ui: { setStatus() {} },
  };
}

test("registered Pi-native mem_search reports native provider transport failure", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";
  globalThis.fetch = async () => {
    throw new Error("connection refused");
  };

  try {
    await installRuntimeStubs();
    const registeredTools = new Map();
    const pluginUrl = pathToFileURL(join(ROOT, "index.ts"));
    pluginUrl.search = `?contract=${Date.now()}`;
    const { default: registerEngram } = await import(pluginUrl.href);
    registerEngram({
      registerTool(tool) {
        registeredTools.set(tool.name, tool);
      },
      on() {},
    });

    const memSearch = registeredTools.get("mem_search");
    assert.ok(memSearch, "mem_search tool should be registered");

    const result = await memSearch.execute(
      "tool-call-1",
      { query: "state markers", project: "gentle-agent-state" },
      undefined,
      undefined,
      {
        cwd: ROOT,
        sessionManager: { getSessionId: () => "test-session" },
        ui: { setStatus() {} },
      },
    );

    assert.equal(result.isError, true);
    assert.match(result.content[0].text, /gentle-engram could not reach the Engram HTTP server/);
    assert.match(result.content[0].text, /Pi-native mem_\* tools are registered/);
    assert.match(result.details.error, /native memory provider is not currently responding/);
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
    await rm(NODE_MODULES, { recursive: true, force: true });
  }
});

test("session-attributed Pi writes bind to acknowledged runtime identity and retry failed registration", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";
  let registrationAttempts = 0;
  const observationBodies = [];
  const sessionBodies = [];
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    if (path === "/health") return { ok: true, async json() { return { status: "ok" }; } };
    if (path === "/project/current") {
      return { ok: true, async json() { return { project: "pi", project_source: "dir_basename", project_path: ROOT }; } };
    }
    if (path === "/sessions") {
      registrationAttempts += 1;
      sessionBodies.push(JSON.parse(init.body));
      if (registrationAttempts === 1) {
        return { ok: false, status: 503, async json() { return { error: "registration unavailable" }; } };
      }
      return { ok: true, status: 201, async json() { return { status: "created" }; } };
    }
    if (path === "/observations") {
      observationBodies.push(JSON.parse(init.body));
      return { ok: true, status: 201, async json() { return { id: observationBodies.length }; } };
    }
    return { ok: true, async json() { return {}; } };
  };

  try {
    await installRuntimeStubs();
    const registeredTools = new Map();
    const pluginUrl = pathToFileURL(join(ROOT, "index.ts"));
    pluginUrl.search = `?binding=${Date.now()}`;
    const { default: registerEngram } = await import(pluginUrl.href);
    registerEngram({
      registerTool(tool) { registeredTools.set(tool.name, tool); },
      on() {},
    });

    const memSave = registeredTools.get("mem_save");
    const ctx = {
      cwd: ROOT,
      sessionManager: { getSessionId: () => "runtime-session" },
      ui: { setStatus() {} },
    };
    const params = { title: "runtime binding", content: "content", session_id: "model-invented" };

    const failed = await memSave.execute("call-1", params, undefined, undefined, ctx);
    assert.equal(failed.isError, true);
    assert.equal(observationBodies.length, 0, "unacknowledged registration must stop the write");

    const succeeded = await memSave.execute("call-2", params, undefined, undefined, ctx);
    assert.equal(succeeded.isError, undefined);
    assert.equal(registrationAttempts, 2, "failed registration must remain retryable");
    assert.equal(sessionBodies[1].id, "runtime-session");
    assert.equal(observationBodies[0].session_id, "runtime-session");
    assert.notEqual(observationBodies[0].session_id, "model-invented");

    await memSave.execute("call-3", params, undefined, undefined, ctx);
    assert.equal(registrationAttempts, 2, "successful acknowledgement should be cached");

    const noRuntime = await memSave.execute(
      "call-4",
      params,
      undefined,
      undefined,
      { ...ctx, sessionManager: { getSessionId: () => undefined } },
    );
    assert.equal(noRuntime.isError, true);
    assert.match(noRuntime.content[0].text, /Pi runtime session ID is unavailable/);
    assert.equal(registrationAttempts, 2, "missing runtime identity must not synthesize or register a session");
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
    await rm(NODE_MODULES, { recursive: true, force: true });
  }
});

test("parallel first-use writes share one acknowledged registration and keep it cached", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";
  const registrationGate = deferred();
  let registrationAttempts = 0;
  const writeRequests = [];
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    if (path === "/health") return { ok: true, async json() { return { status: "ok" }; } };
    if (path === "/project/current") {
      return { ok: true, async json() { return { project: "pi", project_source: "dir_basename", project_path: ROOT }; } };
    }
    if (path === "/sessions") {
      registrationAttempts += 1;
      await registrationGate.promise;
      return { ok: true, status: 201, async json() { return { status: "created" }; } };
    }
    if (path === "/observations") {
      writeRequests.push(JSON.parse(init.body));
      return { ok: true, status: 201, async json() { return { id: writeRequests.length }; } };
    }
    return { ok: true, async json() { return {}; } };
  };

  try {
    const { registeredTools, eventHandlers } = await loadPluginHarness("parallel-success");
    const memSave = registeredTools.get("mem_save");
    const ctx = runtimeContext("parallel-success-session");
    await eventHandlers.get("session_start")({}, ctx);

    const firstWrite = memSave.execute("parallel-success-1", { title: "first", content: "one" }, undefined, undefined, ctx);
    const secondWrite = memSave.execute("parallel-success-2", { title: "second", content: "two" }, undefined, undefined, ctx);
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(registrationAttempts, 1, "parallel first writes must share one registration request");

    registrationGate.resolve();
    const [firstResult, secondResult] = await Promise.all([firstWrite, secondWrite]);
    assert.equal(firstResult.isError, undefined);
    assert.equal(secondResult.isError, undefined);
    assert.deepEqual(writeRequests.map((request) => request.title).sort(), ["first", "second"]);
    assert.ok(writeRequests.every((request) => request.session_id === "parallel-success-session"));

    await memSave.execute("parallel-success-cached", { title: "cached", content: "three" }, undefined, undefined, ctx);
    assert.equal(registrationAttempts, 1, "acknowledged registration must remain cached");
    assert.equal(writeRequests.length, 3);
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
    await rm(NODE_MODULES, { recursive: true, force: true });
  }
});

test("shared registration failure rejects parallel writes and a later call retries", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";
  const registrationGate = deferred();
  let registrationAttempts = 0;
  let registrationShouldFail = true;
  const writeRequests = [];
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    if (path === "/health") return { ok: true, async json() { return { status: "ok" }; } };
    if (path === "/project/current") {
      return { ok: true, async json() { return { project: "pi", project_source: "dir_basename", project_path: ROOT }; } };
    }
    if (path === "/sessions") {
      registrationAttempts += 1;
      if (registrationShouldFail) {
        await registrationGate.promise;
        return { ok: false, status: 503, async json() { return { error: "registration unavailable" }; } };
      }
      return { ok: true, status: 201, async json() { return { status: "created" }; } };
    }
    if (path === "/observations") {
      writeRequests.push(JSON.parse(init.body));
      return { ok: true, status: 201, async json() { return { id: writeRequests.length }; } };
    }
    return { ok: true, async json() { return {}; } };
  };

  try {
    const { registeredTools, eventHandlers } = await loadPluginHarness("parallel-failure");
    const memSave = registeredTools.get("mem_save");
    const ctx = runtimeContext("parallel-failure-session");
    await eventHandlers.get("session_start")({}, ctx);

    const firstWrite = memSave.execute("parallel-failure-1", { title: "first", content: "one" }, undefined, undefined, ctx);
    const secondWrite = memSave.execute("parallel-failure-2", { title: "second", content: "two" }, undefined, undefined, ctx);
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(registrationAttempts, 1, "parallel failed writes must share one registration request");

    registrationGate.resolve();
    const [firstResult, secondResult] = await Promise.all([firstWrite, secondWrite]);
    assert.equal(firstResult.isError, true);
    assert.equal(secondResult.isError, true);
    assert.match(firstResult.content[0].text, /registration unavailable/);
    assert.match(secondResult.content[0].text, /registration unavailable/);
    assert.equal(writeRequests.length, 0, "failed registration must stop every waiting write");

    registrationShouldFail = false;
    const retryResult = await memSave.execute("parallel-failure-retry", { title: "retry", content: "three" }, undefined, undefined, ctx);
    assert.equal(retryResult.isError, undefined);
    assert.equal(registrationAttempts, 2, "a later write must retry failed registration");
    assert.equal(writeRequests.length, 1);

    await memSave.execute("parallel-failure-cached", { title: "cached", content: "four" }, undefined, undefined, ctx);
    assert.equal(registrationAttempts, 2, "successful retry must remain cached");
    assert.equal(writeRequests.length, 2);
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
    await rm(NODE_MODULES, { recursive: true, force: true });
  }
});

test("an opaque runtime session ID stays byte-identical through registration, compaction, and cleanup", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";
  // Pi hands out an opaque session ID. Surrounding whitespace is part of that
  // identity, so normalizing it anywhere would split registration from
  // compaction and strand the cache entry that shutdown tries to clear.
  const runtimeSessionId = "  pi-runtime-session-id  ";
  const sessionBodies = [];
  const observationBodies = [];
  globalThis.fetch = async (url, init) => {
    const path = new URL(url).pathname;
    if (path === "/health") return { ok: true, async json() { return { status: "ok" }; } };
    if (path === "/project/current") {
      return { ok: true, async json() { return { project: "pi", project_source: "dir_basename", project_path: ROOT }; } };
    }
    if (path === "/sessions") {
      sessionBodies.push(JSON.parse(init.body));
      return { ok: true, status: 201, async json() { return { status: "created" }; } };
    }
    if (path === "/observations") {
      observationBodies.push(JSON.parse(init.body));
      return { ok: true, status: 201, async json() { return { id: observationBodies.length }; } };
    }
    if (path === "/context") return { ok: true, async json() { return { context: "" }; } };
    return { ok: true, async json() { return {}; } };
  };

  try {
    const { registeredTools, eventHandlers } = await loadPluginHarness("exact-identity");
    const memSave = registeredTools.get("mem_save");
    const ctx = runtimeContext(runtimeSessionId);
    await eventHandlers.get("session_start")({}, ctx);

    const saved = await memSave.execute("exact-1", { title: "first", content: "one" }, undefined, undefined, ctx);
    assert.equal(saved.isError, undefined);
    assert.equal(sessionBodies.length, 1, "the first write registers the runtime session once");
    assert.equal(sessionBodies[0].id, runtimeSessionId, "registration must use the exact runtime identity");
    assert.equal(observationBodies[0].session_id, runtimeSessionId, "the write must use the exact runtime identity");

    await eventHandlers.get("session_compact")({ summary: "compacted work" }, ctx);
    assert.equal(sessionBodies.length, 1, "compaction must reuse the cached exact identity instead of registering again");
    const compactionSummary = observationBodies.find((body) => body.type === "session_summary");
    assert.ok(compactionSummary, "compaction summary not forwarded");
    assert.equal(compactionSummary.session_id, runtimeSessionId, "compaction must attribute the summary to the exact identity");

    await eventHandlers.get("session_shutdown")({}, ctx);

    const afterShutdown = await memSave.execute("exact-2", { title: "second", content: "two" }, undefined, undefined, ctx);
    assert.equal(afterShutdown.isError, undefined);
    assert.equal(sessionBodies.length, 2, "shutdown must clear the cached entry so nothing is left behind");
    assert.equal(sessionBodies[1].id, runtimeSessionId, "re-registration must still use the exact runtime identity");
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
    await rm(NODE_MODULES, { recursive: true, force: true });
  }
});
