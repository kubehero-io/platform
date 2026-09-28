// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// AlertsService wire shapes (alerts.proto) and view models.

export const RULE_KINDS = ["logs", "cost", "budget", "anomaly", "network", "event"] as const;
export type RuleKind = (typeof RULE_KINDS)[number];

export const OPS = [">", ">=", "<", "<=", "==", "!="] as const;
export type Op = (typeof OPS)[number];

export const SEVERITIES = ["info", "warn", "critical"] as const;
export type AlertSeverity = (typeof SEVERITIES)[number];

export type AlertRuleJson = {
  id?: string;
  name?: string;
  description?: string;
  kind?: string;
  query?: string;
  op?: string;
  threshold?: number;
  pendingFor?: string;
  severity?: string;
  channels?: string[];
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  enabled?: boolean;
  evalInterval?: string;
  createdAt?: string;
  updatedAt?: string;
  createdBy?: string;
};

export type AlertJson = {
  id?: string;
  ruleId?: string;
  ruleName?: string;
  kind?: string;
  state?: string;
  severity?: string;
  labels?: Record<string, string>;
  value?: number;
  summary?: string;
  description?: string;
  startedAt?: string;
  firedAt?: string;
  resolvedAt?: string;
  lastEvalAt?: string;
  silenced?: boolean;
  linkPath?: string;
};

export type SilenceJson = {
  id?: string;
  matchers?: Record<string, string>;
  startsAt?: string;
  endsAt?: string;
  createdBy?: string;
  comment?: string;
};

export type AlertEvaluationJson = { labels?: Record<string, string>; value?: number; triggered?: boolean };
export type TestAlertRuleResponseJson = { results?: AlertEvaluationJson[]; error?: string; execMs?: number };

export type AlertRule = {
  id: string;
  name: string;
  description: string;
  kind: RuleKind;
  query: string;
  op: Op;
  threshold: number;
  pendingFor: string;
  severity: AlertSeverity;
  channels: string[];
  labels: Record<string, string>;
  annotations: Record<string, string>;
  enabled: boolean;
  evalInterval: string;
  createdAt: string;
  updatedAt: string;
  createdBy: string;
};

export type AlertState = "pending" | "firing" | "resolved";

export type Alert = {
  id: string;
  ruleId: string;
  ruleName: string;
  kind: string;
  state: AlertState;
  severity: AlertSeverity;
  labels: Record<string, string>;
  value: number;
  summary: string;
  description: string;
  startedAt: string;
  firedAt: string;
  resolvedAt: string;
  lastEvalAt: string;
  silenced: boolean;
  linkPath: string;
};

export type Silence = {
  id: string;
  matchers: Record<string, string>;
  startsAt: string;
  endsAt: string;
  createdBy: string;
  comment: string;
};

export type Evaluation = { labels: Record<string, string>; value: number; triggered: boolean };
export type TestResult = { results: Evaluation[]; error: string; execMs: number };

/** Draft shape the rule editor edits (strings where the user types). */
export type RuleDraft = {
  id: string;
  name: string;
  description: string;
  kind: RuleKind;
  query: string;
  op: Op;
  threshold: string;
  pendingFor: string;
  severity: AlertSeverity;
  channels: string;
  labels: string;
  summary: string;
  runbookUrl: string;
  enabled: boolean;
  evalInterval: string;
};
