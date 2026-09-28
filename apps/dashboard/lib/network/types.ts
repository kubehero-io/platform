// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// NetworkService wire shapes (observe.proto) and view models.

export type ServiceMapNodeJson = {
  id?: string;
  name?: string;
  namespace?: string;
  kind?: string;
  zone?: string;
  bytesIn?: number;
  bytesOut?: number;
  costUsdMonth?: number;
};

export type ServiceMapEdgeJson = {
  source?: string;
  target?: string;
  port?: number;
  protocol?: string;
  bytes?: number;
  bytesPerSec?: number;
  crossZone?: boolean;
  egress?: boolean;
  costUsdMonth?: number;
  retransmits?: number;
};

export type GetServiceMapResponseJson = {
  nodes?: ServiceMapNodeJson[];
  edges?: ServiceMapEdgeJson[];
  totalCostUsdMonth?: number;
  crossZoneGb?: number;
  egressGb?: number;
  source?: string;
};

export type NetworkCostJson = {
  namespace?: string;
  workload?: string;
  egressGb?: number;
  crossZoneGb?: number;
  egressUsdMonth?: number;
  crossZoneUsdMonth?: number;
  totalUsdMonth?: number;
  topDestination?: string;
};

export type ListNetworkCostsResponseJson = { costs?: NetworkCostJson[]; totalUsdMonth?: number; source?: string };

export type MapNode = {
  id: string;
  name: string;
  namespace: string;
  kind: "workload" | "service" | "node" | "external" | string;
  zone: string;
  bytesIn: number;
  bytesOut: number;
  costUsdMonth: number;
};

export type MapEdge = {
  id: string;
  source: string;
  target: string;
  port: number;
  protocol: string;
  bytes: number;
  bytesPerSec: number;
  crossZone: boolean;
  egress: boolean;
  costUsdMonth: number;
  retransmits: number;
};

export type ServiceMap = {
  nodes: MapNode[];
  edges: MapEdge[];
  totalCostUsdMonth: number;
  crossZoneGb: number;
  egressGb: number;
};

export type NetworkCost = {
  namespace: string;
  workload: string;
  egressGb: number;
  crossZoneGb: number;
  egressUsdMonth: number;
  crossZoneUsdMonth: number;
  totalUsdMonth: number;
  topDestination: string;
};
