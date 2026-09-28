// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { streamMessageToEvents, toInvestigation, SUGGESTED_QUESTIONS } from "./investigate";
import { demoInvestigation, detectIntent } from "@/lib/demo/investigate";

describe("InvestigateStream mapping", () => {
  it("maps each oneof arm to its SSE event", () => {
    expect(streamMessageToEvents({ step: { tool: "query_logs", summary: "ok", durationMs: 12 } })).toEqual([
      { event: "step", data: { tool: "query_logs", input: "", summary: "ok", durationMs: 12, error: false } },
    ]);
    expect(streamMessageToEvents({ progress: "thinking" })).toEqual([{ event: "progress", data: { text: "thinking" } }]);
    expect(streamMessageToEvents({})).toEqual([]);
    const [r] = streamMessageToEvents({ result: { id: "inv-1", answerMarkdown: "## hi", source: "llm", generatedAtUnix: "1790000000" } });
    expect(r.event).toBe("result");
    expect(r.data).toMatchObject({ id: "inv-1", source: "llm", generatedAtUnix: 1_790_000_000, evidence: [], actions: [] });
  });

  it("sanitises evidence links and defaults unknown enums", () => {
    const inv = toInvestigation({
      source: "oracle",
      evidence: [{ kind: "logs", title: "x", linkPath: "https://evil.example" }, { title: "y", linkPath: "/logs?q=1" }],
      actions: [{ title: "Do it", risk: "extreme", kind: "delete.everything" }],
    });
    expect(inv.source).toBe("rules");
    expect(inv.evidence.map((e) => e.linkPath)).toEqual(["", "/logs?q=1"]);
    expect(inv.evidence[1].kind).toBe("note");
    expect(inv.actions[0]).toMatchObject({ risk: "medium", kind: "workload.investigate", status: "proposed" });
  });
});

describe("demo investigation agent", () => {
  it.each([
    ["Why are checkout payments failing right now?", "errors"],
    ["What drove ml-inference spend up this week?", "spend"],
    ["Why did checkout get slower since yesterday?", "latency"],
    ["Which workloads are costing us the most in network egress?", "network"],
    ["Why does cart keep getting OOM-killed?", "oom"],
    ["Where can we save $10k/month without risk?", "savings"],
    ["hello", "overview"],
  ])("%s → %s", (q, intent) => {
    expect(detectIntent(q)).toBe(intent);
  });

  it("answers every suggested question with steps, evidence and a demo source", () => {
    const now = Date.UTC(2026, 8, 28, 14, 7);
    for (const q of SUGGESTED_QUESTIONS) {
      const d = demoInvestigation(q, now);
      expect(d.steps.length).toBeGreaterThan(0);
      expect(d.result.source).toBe("demo");
      expect(d.result.answerMarkdown).toMatch(/##/);
      expect(d.result.evidence.every((e) => e.linkPath === "" || e.linkPath.startsWith("/"))).toBe(true);
      for (const a of d.result.actions) expect(a.status).toBe("proposed");
    }
  });

  it("proposes a memory upsize policy for the OOM question", () => {
    const d = demoInvestigation("Why does cart keep getting OOM-killed?");
    expect(d.result.actions[0].kind).toBe("rightsize.requests");
    expect(d.result.actions[0].crdYaml).toContain("kind: RightsizingPolicy");
  });
});
