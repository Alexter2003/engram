import assert from "node:assert/strict";
import { test } from "node:test";
import { importPluginFromSandbox, withPluginSandbox } from "./plugin-sandbox.mjs";

test("registered Pi-native mem_search reports native provider transport failure", async () => {
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.ENGRAM_URL;
  process.env.ENGRAM_URL = "http://127.0.0.1:17437";
  globalThis.fetch = async () => {
    throw new Error("connection refused");
  };

  try {
    await withPluginSandbox("engram-pi-contract-", async ({ dir, sandbox }) => {
      const registeredTools = new Map();
      const registerEngram = await importPluginFromSandbox(sandbox);
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
          cwd: dir,
          sessionManager: { getSessionId: () => "test-session" },
          ui: { setStatus() {} },
        },
      );

      assert.equal(result.isError, true);
      assert.match(result.content[0].text, /gentle-engram could not reach the Engram HTTP server/);
      assert.match(result.content[0].text, /Pi-native mem_\* tools are registered/);
      assert.match(result.details.error, /native memory provider is not currently responding/);
    });
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.ENGRAM_URL;
    else process.env.ENGRAM_URL = originalUrl;
  }
});
