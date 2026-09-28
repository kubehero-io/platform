// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// The information architecture, in one place: the sidebar renders it,
// ⌘K searches it, and the `g <key>` shortcuts come from it.

import {
  Activity,
  Cpu,
  Flame,
  Gauge,
  Layers,
  MessageCircleQuestion,
  PieChart,
  Receipt,
  Scale,
  ScrollText,
  Settings,
  ShieldAlert,
  ShieldCheck,
  Siren,
  Sparkles,
  Timer,
  Waypoints,
  Wallet,
} from "lucide-react";
import type { ComponentType } from "react";

export type NavBadge = "firing" | "recommendations" | "clusters";

export type NavItem = {
  href: string;
  label: string;
  icon: ComponentType<{ className?: string; style?: React.CSSProperties }>;
  /** Second key of the `g <key>` shortcut. */
  key?: string;
  badge?: NavBadge;
  keywords?: string;
};

export type NavGroup = { label: string; items: NavItem[] };

export const NAV: NavGroup[] = [
  {
    label: "observe",
    items: [
      { href: "/overview", label: "Overview", icon: Gauge, key: "o", keywords: "home command center what changed" },
      { href: "/logs", label: "Logs", icon: ScrollText, key: "l", keywords: "logql loki search tail" },
      { href: "/profiles", label: "Profiles", icon: Flame, key: "p", keywords: "flamegraph pprof cpu continuous profiling" },
      { href: "/network", label: "Network", icon: Waypoints, key: "n", keywords: "service map ebpf egress cross-zone flows" },
      { href: "/fleet", label: "Fleet", icon: Layers, key: "f", badge: "clusters", keywords: "clusters nodes" },
    ],
  },
  {
    label: "cost",
    items: [
      { href: "/allocation", label: "Allocation", icon: PieChart, key: "m", keywords: "opencost spend by namespace team idle shared focus export" },
      { href: "/chargeback", label: "Chargeback", icon: Receipt, keywords: "teams cost centers showback" },
      { href: "/rightsizing", label: "Rightsizing", icon: Scale, key: "r", badge: "recommendations", keywords: "waste requests vpa recommendations" },
      { href: "/gpu", label: "GPU", icon: Cpu, keywords: "a100 h100 utilization" },
      { href: "/capacity", label: "Capacity", icon: Timer, keywords: "pending pods unschedulable" },
    ],
  },
  {
    label: "control",
    items: [
      { href: "/alerts", label: "Alerts", icon: Siren, key: "e", badge: "firing", keywords: "rules silences firing pending notifications" },
      { href: "/budgets", label: "Budgets", icon: Wallet, key: "b", keywords: "budget ceiling policies arm kill-switch" },
      { href: "/ceilings", label: "Ceiling log", icon: ShieldCheck, keywords: "audit log policy decisions undo" },
    ],
  },
  {
    label: "agents",
    items: [
      { href: "/advisor", label: "Advisor", icon: Sparkles, key: "v", keywords: "briefing daily voice" },
      { href: "/ask", label: "Ask KubeHero", icon: MessageCircleQuestion, key: "k", keywords: "investigate question agent chat why" },
    ],
  },
  {
    label: "security",
    items: [{ href: "/posture", label: "Posture", icon: ShieldAlert, keywords: "cve vulnerabilities trivy" }],
  },
  {
    label: "",
    items: [{ href: "/settings", label: "Settings", icon: Settings, key: "s", keywords: "integrations tokens" }],
  },
];

export const NAV_ITEMS: NavItem[] = NAV.flatMap((g) => g.items);

/** `g <key>` → href */
export const G_SHORTCUTS: Record<string, string> = Object.fromEntries(NAV_ITEMS.filter((i) => i.key).map((i) => [i.key!, i.href]));

export const ACTIVITY_ICON = Activity;
