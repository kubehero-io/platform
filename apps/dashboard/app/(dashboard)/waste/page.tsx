// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// The waste list merged into /rightsizing (same data, per-container
// percentiles, and a guarded policy instead of a fake "apply" button).
// Kept as a redirect so old links and bookmarks keep working.

import { redirect } from "next/navigation";
import { flatParams, type SearchParamsRecord } from "@/lib/url";

export default async function WastePage({ searchParams }: { searchParams: Promise<SearchParamsRecord> }) {
  const p = flatParams(await searchParams);
  const keep = new URLSearchParams();
  if (p.q) keep.set("q", p.q);
  const qs = keep.toString();
  redirect(qs ? `/rightsizing?${qs}` : "/rightsizing");
}
