// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// NetworkService (eBPF service map + network costs) with the
// degrade-to-demo contract.

import "server-only";
import { demoNetworkCosts, demoServiceMap } from "@/lib/demo/network";
import { toNetworkCosts, toServiceMap } from "@/lib/network/map";
import type { GetServiceMapResponseJson, ListNetworkCostsResponseJson, NetworkCost, ServiceMap } from "@/lib/network/types";
import { callUnary, type RpcError } from "./rpc";
import { demo, live, type Sourced } from "./source";

const SERVICE = "kubehero.v1.NetworkService";

function degrade<T>(err: RpcError, fixture: () => T): Sourced<T> {
  return err.code === "not_configured" ? demo(fixture(), "unset") : demo(fixture(), "error", `${err.code}: ${err.message}`.slice(0, 160));
}

type Range = { startMs: number; endMs: number };

export async function getServiceMap(q: { clusterId?: string; namespace?: string; range: Range; maxNodes?: number; forceDemo?: boolean }): Promise<Sourced<ServiceMap>> {
  const fixture = () => demoServiceMap({ clusterId: q.clusterId, namespace: q.namespace, ...q.range });
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<GetServiceMapResponseJson>("cp", SERVICE, "GetServiceMap", {
    clusterId: q.clusterId ?? "",
    namespace: q.namespace ?? "",
    startUnixMs: String(Math.floor(q.range.startMs)),
    endUnixMs: String(Math.floor(q.range.endMs)),
    maxNodes: q.maxNodes ?? 60,
  });
  if (!res.ok) return degrade(res.error, fixture);
  const map = toServiceMap(res.data);
  if (res.data.source === "demo") return demo(map, "upstream");
  return live(map);
}

export async function listNetworkCosts(q: { clusterId?: string; range: Range; limit?: number; forceDemo?: boolean }): Promise<Sourced<{ costs: NetworkCost[]; totalUsdMonth: number }>> {
  const fixture = () => demoNetworkCosts({ clusterId: q.clusterId, ...q.range, limit: q.limit });
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<ListNetworkCostsResponseJson>("cp", SERVICE, "ListNetworkCosts", {
    clusterId: q.clusterId ?? "",
    startUnixMs: String(Math.floor(q.range.startMs)),
    endUnixMs: String(Math.floor(q.range.endMs)),
    limit: q.limit ?? 100,
  });
  if (!res.ok) return degrade(res.error, fixture);
  const v = toNetworkCosts(res.data);
  if (res.data.source === "demo") return demo(v, "upstream");
  return live(v);
}
