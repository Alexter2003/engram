// Behavioral coverage for Pi startup: a real fake `engram` binary is spawned as a child
// process, so these tests exercise the actual spawn/readiness/failure lifecycle instead of
// re-implementing it with stubs.
import assert from "node:assert/strict";
import { createServer } from "node:net";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
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

function freePort() {
  return new Promise((resolve, reject) => {
    const probe = createServer();
    probe.once("error", reject);
    probe.listen(0, "127.0.0.1", () => {
      const { port } = probe.address();
      probe.close(() => resolve(port));
    });
  });
}

// A fake `engram serve` that logs every invocation, then either dies before readiness or
// starts answering /health after `readyAfterMs` — the slow-health window under test.
async function writeFakeEngramBin(dir, { spawnLog, port, readyAfterMs, exitCode }) {
  const binPath = join(dir, "fake-engram.mjs");
  const script = `#!/usr/bin/env node
import { appendFileSync } from "node:fs";
import { createServer } from "node:http";

if (process.argv[2] === "serve") {
  appendFileSync(${JSON.stringify(spawnLog)}, "serve\\n");
}
${exitCode === undefined
      ? `const server = createServer((req, res) => {
  if (req.url.startsWith("/project/current")) {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ project: "fake-project" }));
    return;
  }
  res.writeHead(200, { "content-type": "application/json" });
  res.end("{}");
});
setTimeout(() => {
  server.listen(${port}, "127.0.0.1");
  // Never outlive the test run, even if the parent forgets this detached child.
  setTimeout(() => { server.close(); process.exit(0); }, 5000);
}, ${readyAfterMs});`
      : `process.exit(${exitCode});`}
`;
  await writeFile(binPath, script, "utf8");
  await chmod(binPath, 0o755);
  return binPath;
}

async function loadPlugin({ engramBin, port, cwd }) {
  await installRuntimeStubs();
  delete process.env.ENGRAM_URL;
  process.env.ENGRAM_BIN = engramBin;
  process.env.ENGRAM_PORT = String(port);

  const pluginUrl = pathToFileURL(join(ROOT, "index.ts"));
  pluginUrl.search = `?startup=${Date.now()}-${Math.random()}`;
  const { default: registerEngram } = await import(pluginUrl.href);

  const tools = new Map();
  const hooks = new Map();
  registerEngram({
    registerTool(tool) {
      tools.set(tool.name, tool);
    },
    on(name, handler) {
      hooks.set(name, handler);
    },
  });

  const ctx = { cwd, sessionManager: { getSessionId: () => "session-startup" } };
  return { tools, hooks, ctx };
}

async function withFixture(options, run) {
  const dir = await mkdtemp(join(tmpdir(), "engram-pi-startup-"));
  const originalBin = process.env.ENGRAM_BIN;
  const originalPort = process.env.ENGRAM_PORT;
  const originalUrl = process.env.ENGRAM_URL;
  try {
    const spawnLog = join(dir, "spawns.log");
    await writeFile(spawnLog, "", "utf8");
    const port = await freePort();
    const engramBin = options.missingBin
      ? join(dir, "engram-does-not-exist")
      : await writeFakeEngramBin(dir, { spawnLog, port, readyAfterMs: options.readyAfterMs ?? 0, exitCode: options.exitCode });
    const plugin = await loadPlugin({ engramBin, port, cwd: dir });
    await run({ ...plugin, spawnLog, dir, port });
  } finally {
    if (originalBin === undefined) delete process.env.ENGRAM_BIN; else process.env.ENGRAM_BIN = originalBin;
    if (originalPort === undefined) delete process.env.ENGRAM_PORT; else process.env.ENGRAM_PORT = originalPort;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL; else process.env.ENGRAM_URL = originalUrl;
    await rm(dir, { recursive: true, force: true });
  }
}

async function countSpawns(spawnLog) {
  const log = await readFile(spawnLog, "utf8");
  return log.split("\n").filter((line) => line === "serve").length;
}

test("a slow health probe never authorizes a duplicate spawn", async () => {
  await withFixture({ readyAfterMs: 600 }, async ({ hooks, ctx, spawnLog }) => {
    const sessionStart = hooks.get("session_start");
    const beforeAgentStart = hooks.get("before_agent_start");
    assert.ok(sessionStart && beforeAgentStart, "startup hooks are registered");

    // Both hooks race into initialization while the child is still starting up.
    await Promise.all([
      sessionStart({}, ctx),
      beforeAgentStart({ systemPrompt: "base", prompt: "hi" }, ctx),
    ]);

    assert.equal(await countSpawns(spawnLog), 1, "concurrent hooks share one spawned server");
  });
});

test("a child that exits before readiness surfaces a normalized tool error", async () => {
  await withFixture({ exitCode: 1 }, async ({ tools, ctx }) => {
    const memSearch = tools.get("mem_search");
    assert.ok(memSearch, "mem_search is registered");

    const result = await memSearch.execute("call-1", { query: "startup" }, undefined, undefined, ctx);

    assert.equal(result.isError, true, "a failed startup is a tool error, not a rejection");
    assert.match(result.content[0].text, /could not initialize the Engram memory provider/);
  });
});

test("a child that exits before readiness never escapes the session hooks", async () => {
  await withFixture({ exitCode: 1 }, async ({ hooks, ctx }) => {
    await assert.doesNotReject(hooks.get("session_start")({}, ctx));
    await assert.doesNotReject(hooks.get("session_compact")({}, ctx));
    await assert.doesNotReject(hooks.get("tool_execution_end")({ toolName: "Read" }, ctx));

    const result = await hooks.get("before_agent_start")({ systemPrompt: "base", prompt: "hello there" }, ctx);
    assert.match(result.systemPrompt, /^base\n\n/, "memory instructions still reach the agent");
  });
});

test("a child that cannot be spawned surfaces a normalized error through tools and hooks", async () => {
  await withFixture({ missingBin: true }, async ({ tools, hooks, ctx }) => {
    await assert.doesNotReject(hooks.get("session_start")({}, ctx));

    const result = await tools.get("mem_save").execute("call-2", { content: "x" }, undefined, undefined, ctx);
    assert.equal(result.isError, true);
    assert.match(result.content[0].text, /could not initialize the Engram memory provider/);
  });
});
