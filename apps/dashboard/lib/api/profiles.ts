// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// ProfilesService with the degrade-to-demo contract. An install that has
// no profiles yet gets an honest empty state with setup instructions on
// /profiles (never silent fixtures) — `?demo=1` previews the fixtures.

import "server-only";
import { demoFlamegraph, demoProfileTargets, demoTopFunctions } from "@/lib/demo/profiles";
import { toFlamegraph, toProfileTarget, toTopFunctions } from "@/lib/profiles/map";
import type {
  Flamegraph,
  GetFlamegraphResponseJson,
  GetTopFunctionsResponseJson,
  ProfileTarget,
  ProfileTargetJson,
  TopFunction,
} from "@/lib/profiles/types";
import { callUnary, type RpcError } from "./rpc";
import { demo, live, type Sourced } from "./source";

const SERVICE = "kubehero.v1.ProfilesService";

function degrade<T>(err: RpcError, fixture: () => T): Sourced<T> {
  return err.code === "not_configured" ? demo(fixture(), "unset") : demo(fixture(), "error", `${err.code}: ${err.message}`.slice(0, 160));
}

type Range = { startMs: number; endMs: number };

export async function listProfileTargets(q: { range: Range; namespace?: string; clusterId?: string; forceDemo?: boolean }): Promise<Sourced<ProfileTarget[]>> {
  const fixture = () => demoProfileTargets(q.range.endMs, q.namespace);
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<{ targets?: ProfileTargetJson[] }>("cp", SERVICE, "ListProfileTargets", {
    startUnixMs: String(Math.floor(q.range.startMs)),
    endUnixMs: String(Math.floor(q.range.endMs)),
    clusterId: q.clusterId ?? "",
    namespace: q.namespace ?? "",
  });
  if (!res.ok) return degrade(res.error, fixture);
  return live((res.data.targets ?? []).map(toProfileTarget).sort((a, b) => b.costUsdMonth - a.costUsdMonth));
}

export type FlameQuery = {
  service: string;
  namespace?: string;
  type: string;
  range: Range;
  baseline?: Range;
  clusterId?: string;
  forceDemo?: boolean;
};

export async function getFlamegraph(q: FlameQuery): Promise<Sourced<Flamegraph | null>> {
  const fixture = () =>
    demoFlamegraph({ service: q.service, namespace: q.namespace, type: q.type, startMs: q.range.startMs, endMs: q.range.endMs, diff: !!q.baseline });
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<GetFlamegraphResponseJson>(
    "cp",
    SERVICE,
    "GetFlamegraph",
    {
      selector: { service: q.service, type: q.type, namespace: q.namespace ?? "", clusterId: q.clusterId ?? "" },
      startUnixMs: String(Math.floor(q.range.startMs)),
      endUnixMs: String(Math.floor(q.range.endMs)),
      baselineStartUnixMs: q.baseline ? String(Math.floor(q.baseline.startMs)) : "0",
      baselineEndUnixMs: q.baseline ? String(Math.floor(q.baseline.endMs)) : "0",
      maxNodes: 2048,
    },
    { timeoutMs: 20_000 },
  );
  if (!res.ok) return degrade(res.error, fixture);
  const fg = toFlamegraph(res.data);
  return live(fg.nodes.length > 0 ? fg : null);
}

export async function getTopFunctions(q: FlameQuery & { limit?: number; orderBy?: "self" | "total" }): Promise<Sourced<TopFunction[]>> {
  const fixture = () =>
    demoTopFunctions({ service: q.service, namespace: q.namespace, type: q.type, startMs: q.range.startMs, endMs: q.range.endMs, limit: q.limit, orderBy: q.orderBy });
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<GetTopFunctionsResponseJson>("cp", SERVICE, "GetTopFunctions", {
    selector: { service: q.service, type: q.type, namespace: q.namespace ?? "", clusterId: q.clusterId ?? "" },
    startUnixMs: String(Math.floor(q.range.startMs)),
    endUnixMs: String(Math.floor(q.range.endMs)),
    limit: q.limit ?? 25,
    orderBy: q.orderBy ?? "self",
  });
  if (!res.ok) return degrade(res.error, fixture);
  return live(toTopFunctions(res.data));
}
