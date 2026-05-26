// SPDX-License-Identifier: BUSL-1.1
package engine

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// needsArming reads spec.humanArm from a policy's JSON spec. Default is
// true (requires arming) when the field is absent or null.
func needsArming(spec []byte) (bool, error) {
	var probe struct {
		HumanArm *bool `json:"humanArm"`
	}
	if err := json.Unmarshal(spec, &probe); err != nil {
		return true, err
	}
	if probe.HumanArm == nil {
		return true, nil
	}
	return *probe.HumanArm, nil
}

var ceilingRe = regexp.MustCompile(`^\$?([0-9][0-9,]*(?:\.[0-9]+)?)(?:\s*/?\s*(mo|month|hr|hour))?$`)

// parseCeiling reads strings like "$100000/mo" or "$300/hr" or "150000"
// and returns the USD amount normalized to month-to-date dollars.
// "/hr" values assume 30-day month for normalization.
func parseCeiling(s string) float64 {
	s = strings.TrimSpace(s)
	m := ceilingRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	raw := strings.ReplaceAll(m[1], ",", "")
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(m[2]) {
	case "hr", "hour":
		return v * 24 * 30
	default:
		return v
	}
}
