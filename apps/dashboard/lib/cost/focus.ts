// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// FinOps FOCUS 1.2 rows for DEMO mode only. Live exports stream straight
// from the control plane's /api/v1/export/focus (it owns the billing
// semantics); this generator exists so the export button does something
// honest without one — every row says x_KubeHeroSource=demo.

import type { AllocationRow } from "./types";
import { csvField } from "./allocation";

export const FOCUS_COLUMNS = [
  "BilledCost", "EffectiveCost", "ListCost", "ContractedCost", "BillingCurrency", "ChargePeriodStart",
  "ChargePeriodEnd", "ChargeCategory", "ChargeClass", "ChargeDescription", "ConsumedQuantity", "ConsumedUnit",
  "PricingQuantity", "PricingUnit", "ProviderName", "PublisherName", "InvoiceIssuerName", "RegionId", "RegionName",
  "AvailabilityZone", "ResourceId", "ResourceName", "ResourceType", "ServiceCategory", "ServiceName", "SubAccountId",
  "SubAccountName", "Tags", "x_IdleCost", "x_SharedCost", "x_NetworkCost", "x_CpuEfficiency", "x_RamEfficiency",
  "x_KubeHeroSource",
] as const;

const PROVIDER: Record<string, [string, string]> = {
  eks: ["AWS", "us-east-1"],
  gke: ["Google Cloud", "us-central1"],
  aks: ["Microsoft Azure", "westeurope"],
};

export function focusCsv(rows: AllocationRow[], startMs: number, endMs: number, source: string): string {
  const out = [FOCUS_COLUMNS.join(",")];
  const start = new Date(startMs).toISOString();
  const end = new Date(endMs).toISOString();
  const hours = Math.max(1 / 60, (endMs - startMs) / 3_600_000);
  for (const r of rows) {
    if (r.isIdle) continue;
    const cluster = r.properties.cluster ?? "";
    const [provider, region] = PROVIDER[cluster.slice(0, 3)] ?? ["Kubernetes", ""];
    const ns = r.properties.namespace ?? "";
    const workload = r.properties.workload ?? r.name;
    const cost = r.totalCost;
    const coreHours = r.cpuCoreRequestAverage * hours;
    out.push(
      [
        cost, cost, cost, cost, "USD", start, end, "Usage", "", `Kubernetes allocation for ${r.name}`,
        coreHours, "Core-Hours", coreHours, "Core-Hours", provider, "KubeHero", "KubeHero (allocated)", region, region,
        r.properties.zone ?? "", [cluster, ns, workload].filter(Boolean).join("/"), r.name, "Kubernetes Workload",
        "Compute", "Kubernetes", cluster, cluster,
        JSON.stringify({ namespace: ns, workload, team: r.properties.team ?? "" }),
        r.idleCost, r.sharedCost, r.networkCost, r.cpuEfficiency, r.ramEfficiency, source,
      ]
        .map(csvField)
        .join(","),
    );
  }
  return out.join("\r\n") + "\r\n";
}
