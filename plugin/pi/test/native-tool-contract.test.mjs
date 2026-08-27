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

// Loads the extension in isolation and returns the tools it registers. Each call gets a fresh
// module instance so module-level project/session state cannot leak between tests.
async function loadRegisteredTools() {
  const registeredTools = new Map();
  const pluginUrl = pathToFileURL(join(ROOT, "index.ts"));
  pluginUrl.search = `?contract=${Date.now()}-${Math.random()}`;
  const { default: registerEngram } = await import(pluginUrl.href);
  registerEngram({
    registerTool(tool) {
      registeredTools.set(tool.name, tool);
    },
    on() {},
  });
  return registeredTools;
}

// Records every request the extension issues so a test can assert the wire contract the Engram
// HTTP server actually receives, instead of asserting over the extension source text.
function recordingFetch(routes) {
  const calls = [];
  const fetchStub = async (url, init = {}) => {
    const method = init.method ?? "GET";
    const path = new URL(url).pathname + new URL(url).search;
    const body = init.body ? JSON.parse(init.body) : undefined;
    calls.push({ method, path, body });
    const route = routes.find((candidate) => candidate.method === method && path.startsWith(candidate.path));
    const status = route?.status ?? 200;
    const payload = route?.body ?? {};
    return new Response(JSON.stringify(payload), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  };
  return { calls, fetchStub };
}

test("registered Pi-native mem_save_prompt persists through the Engram /prompts endpoint", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";

  // The server assigns prompt ids from user_prompts, a table whose sequence is independent of
  // observations. Issue #706 read one of these low ids as an observation id; the response must
  // therefore name the namespace it belongs to.
  const serverAssignedPromptID = 213;
  const { calls, fetchStub } = recordingFetch([
    { method: "GET", path: "/health", body: { status: "ok" } },
    { method: "GET", path: "/project/current", body: { project: "paidosdep" } },
    { method: "POST", path: "/sessions", body: { status: "ok" } },
    { method: "POST", path: "/prompts", status: 201, body: { id: serverAssignedPromptID, status: "saved" } },
  ]);
  globalThis.fetch = fetchStub;

  try {
    await installRuntimeStubs();
    const registeredTools = await loadRegisteredTools();
    const memSavePrompt = registeredTools.get("mem_save_prompt");
    assert.ok(memSavePrompt, "mem_save_prompt tool should be registered");

    const result = await memSavePrompt.execute(
      "tool-call-prompt",
      { content: "preserve this exact user prompt", project: "paidosdep" },
      undefined,
      undefined,
      {
        cwd: ROOT,
        sessionManager: { getSessionId: () => "test-session" },
        ui: { setStatus() {} },
      },
    );

    assert.notEqual(result.isError, true, "a successful prompt save must not surface as a tool error");

    // The prompt must reach POST /prompts carrying the requested project scope, and the session it
    // references must have been created under that same project first.
    const promptCall = calls.find((call) => call.method === "POST" && call.path === "/prompts");
    assert.ok(promptCall, "mem_save_prompt must POST to /prompts");
    assert.equal(promptCall.body.project, "paidosdep");
    assert.equal(promptCall.body.content, "preserve this exact user prompt");
    assert.ok(promptCall.body.session_id, "the prompt must be attributed to a session");

    const sessionCall = calls.find((call) => call.method === "POST" && call.path === "/sessions");
    assert.ok(sessionCall, "mem_save_prompt must ensure its session exists before writing");
    assert.equal(sessionCall.body.project, "paidosdep");
    assert.equal(sessionCall.body.id, promptCall.body.session_id);
    assert.ok(
      calls.indexOf(sessionCall) < calls.indexOf(promptCall),
      "the session must be created before the prompt that references it",
    );

    // The returned identity is prompt-scoped: it echoes the id the server assigned, and it is not
    // offered under a name that mem_get_observation would accept.
    assert.deepEqual(result.details.data, { prompt_id: serverAssignedPromptID, status: "saved" });
    assert.equal(result.details.data.id, undefined, "an observation-shaped id must not be returned");
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
    await rm(NODE_MODULES, { recursive: true, force: true });
  }
});

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
