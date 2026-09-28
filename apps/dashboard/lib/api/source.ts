// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Provenance for every dataset a page renders. Pages never crash on a
// failed RPC: they serve demo fixtures and say why (DataSourceBadge).

export type DataSource = "live" | "demo";

export type DemoReason =
  | "unset" //    no control plane configured
  | "error" //    the RPC failed (unreachable, timeout, 5xx, unimplemented…)
  | "empty" //    live, but nothing ingested yet — fixtures show what it will look like
  | "upstream" // the control plane answered with its own demo fixtures
  | "forced"; //  ?demo=1 preview on a live install

export type Sourced<T> = {
  data: T;
  source: DataSource;
  reason?: DemoReason;
  /** Short error detail for the badge tooltip (never secrets). */
  detail?: string;
};

export function live<T>(data: T): Sourced<T> {
  return { data, source: "live" };
}

export function demo<T>(data: T, reason: DemoReason, detail?: string): Sourced<T> {
  return { data, source: "demo", reason, detail };
}

/** Collapse several datasets' provenance into one header badge. */
export function combineSources(...xs: { source: DataSource; reason?: DemoReason; detail?: string }[]): {
  source: DataSource;
  reason?: DemoReason;
  detail?: string;
} {
  const demoOne = xs.find((x) => x.source === "demo" && x.reason === "error") ?? xs.find((x) => x.source === "demo");
  return demoOne ? { source: "demo", reason: demoOne.reason, detail: demoOne.detail } : { source: "live" };
}
