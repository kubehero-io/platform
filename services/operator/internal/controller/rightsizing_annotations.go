// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Workload annotations the rightsizer reads and writes. They live on the
// Deployment / StatefulSet (not the pod template) so bookkeeping never
// triggers a rollout on its own.
const (
	// AnnotationRightsizing = "disabled" opts a workload (or a whole
	// namespace) out of every RightsizingPolicy.
	AnnotationRightsizing = "kubehero.io/rightsizing"

	// AnnotationRightsizePrevious holds the undo history (JSON, see
	// RightsizeHistory): what each applied change replaced.
	// `kubehero undo <changeId>` restores from it.
	AnnotationRightsizePrevious = "kubehero.io/rightsize-previous"

	// AnnotationRightsizeReverted is stamped by `kubehero undo`. A human
	// reverting a change is the strongest possible "no" — the operator
	// leaves the workload alone until someone removes the annotation.
	AnnotationRightsizeReverted = "kubehero.io/rightsize-reverted"

	// annotationRightsizedAtPrefix + "YYYY-MM-DD" counts changes applied
	// on that UTC day (safety.maxChangePerDay). Only today's key is kept.
	annotationRightsizedAtPrefix = "kubehero.io/rightsized-at-"

	// maxHistory bounds the undo history kept per workload.
	maxHistory = 5
)

// ResourceValues is a container's requests + limits for the resources
// the rightsizer manages (cpu, memory), as Kubernetes quantity strings.
type ResourceValues struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// ContainerChange records one container's before/after values. Only the
// keys that changed appear in Applied; Previous carries the value each
// of those keys had before (the undo target).
type ContainerChange struct {
	Name     string         `json:"name"`
	Previous ResourceValues `json:"previous"`
	Applied  ResourceValues `json:"applied"`
}

// RightsizeChange is one applied patch to one workload.
type RightsizeChange struct {
	ChangeID   string            `json:"changeId"`
	AppliedAt  string            `json:"appliedAt"` // RFC3339, UTC
	Policy     string            `json:"policy"`    // <namespace>/<name>
	Containers []ContainerChange `json:"containers"`
}

// RightsizeHistory is the JSON document stored in
// AnnotationRightsizePrevious — newest change first, at most maxHistory.
// The CLI (cli/kubehero/cmd/undo.go) mirrors this shape; keep them in
// sync (the version field guards against drift).
type RightsizeHistory struct {
	Version int               `json:"version"`
	Changes []RightsizeChange `json:"changes"`
}

// parseHistory reads the undo history. A missing or unreadable
// annotation yields an empty history rather than an error: the
// annotation is advisory bookkeeping and a hand-edited value must never
// wedge the reconciler.
func parseHistory(annotations map[string]string) RightsizeHistory {
	h := RightsizeHistory{Version: 1}
	raw := annotations[AnnotationRightsizePrevious]
	if raw == "" {
		return h
	}
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return RightsizeHistory{Version: 1}
	}
	h.Version = 1
	return h
}

// withChange returns the history with c prepended, trimmed to maxHistory.
func (h RightsizeHistory) withChange(c RightsizeChange) RightsizeHistory {
	out := RightsizeHistory{Version: 1, Changes: append([]RightsizeChange{c}, h.Changes...)}
	if len(out.Changes) > maxHistory {
		out.Changes = out.Changes[:maxHistory]
	}
	return out
}

func (h RightsizeHistory) marshal() (string, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("marshal rightsize history: %w", err)
	}
	return string(b), nil
}

// dayKey is the per-day change-counter annotation for t's UTC date.
func dayKey(t time.Time) string {
	return annotationRightsizedAtPrefix + t.UTC().Format("2006-01-02")
}

// changesToday reads today's change counter. Only the operator writes
// it, so an unreadable value most likely means a human reset it by hand
// and it counts as zero.
func changesToday(annotations map[string]string, now time.Time) int32 {
	v, ok := annotations[dayKey(now)]
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
	if err != nil || n < 0 {
		return 0
	}
	return int32(n)
}

// counterPatch returns the annotation updates that bump today's counter
// and drop stale day keys (nil value = delete in a merge patch).
func counterPatch(annotations map[string]string, now time.Time) map[string]*string {
	out := map[string]*string{}
	today := dayKey(now)
	for k := range annotations {
		if strings.HasPrefix(k, annotationRightsizedAtPrefix) && k != today {
			out[k] = nil
		}
	}
	n := strconv.Itoa(int(changesToday(annotations, now)) + 1)
	out[today] = &n
	return out
}

// newChangeID returns an id like "rs-5f2c9a1e". It is the handle humans
// use with `kubehero undo`, so it stays short and copy-pasteable.
func newChangeID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to
		// time so an id is still unique enough to find in the history.
		return fmt.Sprintf("rs-%08x", uint32(time.Now().UnixNano()))
	}
	return "rs-" + hex.EncodeToString(b)
}
