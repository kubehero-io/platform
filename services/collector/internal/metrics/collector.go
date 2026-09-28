// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package metrics

// Collector self-telemetry. Every pipeline reports here so one scrape
// answers "is this node agent shipping, and if not, where is it losing
// data?". Label values are bounded:
//
//	signal: cost | usage | events | logs | profiles | flows
//	reason: queue_full | rejected | retries_exhausted | server | shutdown |
//	        rate_limited | buffer_full | truncated | no_sink | … (per pipeline)
//	code:   Connect error codes (unavailable, deadline_exceeded, …)
var (
	ItemsSent = Default.NewCounterVec("kubehero_collector_items_sent_total",
		"Items (pod/node cost samples, log lines, profiles, flows, usage rows, events) accepted by the control plane.",
		"signal")
	ItemsDropped = Default.NewCounterVec("kubehero_collector_items_dropped_total",
		"Items the collector gave up on, by reason. queue_full = bounded queue overflow during a control-plane outage (oldest dropped first).",
		"signal", "reason")
	PayloadBytes = Default.NewCounterVec("kubehero_collector_payload_bytes_total",
		"Uncompressed protobuf bytes of successfully shipped requests (on the wire they are gzip-compressed).",
		"signal")
	SendErrors = Default.NewCounterVec("kubehero_collector_send_errors_total",
		"Failed control-plane calls (each retry attempt counts), by Connect error code.",
		"signal", "code")
	QueueItems = Default.NewGaugeVec("kubehero_collector_queue_items",
		"Items waiting in the in-memory ship queue.",
		"signal")
	QueueBytes = Default.NewGaugeVec("kubehero_collector_queue_bytes",
		"Bytes waiting in the in-memory ship queue.",
		"signal")
	LastSuccess = Default.NewGaugeVec("kubehero_collector_last_success_timestamp_seconds",
		"Unix time of the last successful control-plane call.",
		"signal")
	Leader = Default.NewGaugeVec("kubehero_collector_leader",
		"1 while this collector holds the kubehero-collector lease and runs cluster-scoped duties.")

	ScanDuration = Default.NewGaugeVec("kubehero_collector_scan_duration_seconds",
		"Wall time of the last scan, by loop.",
		"loop")
	ScanErrors = Default.NewCounterVec("kubehero_collector_scan_errors_total",
		"Scan-loop failures (kubelet stats unavailable, informer not synced, list errors), by loop and reason.",
		"loop", "reason")
	PodsObserved = Default.NewGaugeVec("kubehero_collector_pods",
		"Running pods priced in the last cost scan.")

	EventsEmitted = Default.NewCounterVec("kubehero_collector_events_emitted_total",
		"Cluster events detected and queued, by kind and source (status = node-local container status, k8s = Warning events, pending = unschedulable scan, node = node conditions).",
		"kind", "source")

	LogLinesRead = Default.NewCounterVec("kubehero_collector_log_lines_read_total",
		"Complete log lines read from /var/log/pods.")
	LogBytesRead = Default.NewCounterVec("kubehero_collector_log_bytes_read_total",
		"Bytes read from /var/log/pods.")
	LogLinesDropped = Default.NewCounterVec("kubehero_collector_log_lines_dropped_total",
		"Log lines dropped before shipping, by reason (rate_limited, excluded, parse_error).",
		"reason")
	LogLinesTruncated = Default.NewCounterVec("kubehero_collector_log_lines_truncated_total",
		"Log lines cut at the 64 KiB line cap.")
	LogFiles = Default.NewGaugeVec("kubehero_collector_log_files",
		"Log files currently tailed.")
	LogRotations = Default.NewCounterVec("kubehero_collector_log_file_events_total",
		"Tailer file lifecycle events: rotated, truncated, removed, reopened.",
		"event")

	ProfileScrapes = Default.NewCounterVec("kubehero_collector_profile_scrapes_total",
		"pprof scrapes by profile type and outcome (ok | error | too_large).",
		"type", "outcome")

	EBPFStatus = Default.NewGaugeVec("kubehero_collector_ebpf_enabled",
		"1 when eBPF programs were requested and ebpf.Start succeeded for the given subsystem.",
		"subsystem")
	EBPFAttached = Default.NewGaugeVec("kubehero_collector_ebpf_attached",
		"1 while the given eBPF program is attached (from ebpf.Stats()).",
		"program")
	EBPFCounters = Default.NewCounterVec("kubehero_collector_ebpf_total",
		"Cumulative eBPF pipeline counters from ebpf.Stats(): flows_emitted, samples_drained, drain_errors, map_full_events, ….",
		"counter")
)

// PublishEBPF copies a snapshot of the ebpf package's cumulative
// counters into the registry; call it on every scrape. Keys become label
// values, so pass the fixed field names of ebpf.Counters only.
func PublishEBPF(attached map[string]bool, counters map[string]uint64) {
	for k, v := range attached {
		g := EBPFAttached.With(k)
		if v {
			g.Set(1)
		} else {
			g.Set(0)
		}
	}
	for k, v := range counters {
		EBPFCounters.With(k).Set(float64(v))
	}
}
