// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehero-io/platform/cli/kubehero/internal/client"
	"github.com/kubehero-io/platform/cli/kubehero/internal/kube"
)

// Annotations shared with the operator (services/operator/internal/
// controller/rightsizing_annotations.go). The JSON shape below mirrors
// its RightsizeHistory; the version field guards against drift.
const (
	annRightsizePrevious = "kubehero.io/rightsize-previous"
	annRightsizeReverted = "kubehero.io/rightsize-reverted"
	undoFieldManager     = "kubehero-cli"
)

var changeIDRE = regexp.MustCompile(`^rs-[0-9a-f]{8}$`)

type resourceValues struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type containerChange struct {
	Name     string         `json:"name"`
	Previous resourceValues `json:"previous"`
	Applied  resourceValues `json:"applied"`
}

type rightsizeChange struct {
	ChangeID   string            `json:"changeId"`
	AppliedAt  string            `json:"appliedAt"`
	Policy     string            `json:"policy"`
	Containers []containerChange `json:"containers"`
}

type rightsizeHistory struct {
	Version int               `json:"version"`
	Changes []rightsizeChange `json:"changes"`
}

// workloadObj is the slice of a Deployment / StatefulSet undo reads.
type workloadObj struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		ResourceVersion string            `json:"resourceVersion"`
		Annotations     map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name      string         `json:"name"`
					Resources resourceValues `json:"resources"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func undoCmd() *cobra.Command {
	var namespace, kubeconfig, kubeContext string
	var force, dryRun bool
	c := &cobra.Command{
		Use:   "undo <change-id>",
		Short: "Restore the resources a rightsizing change replaced",
		Long: `Undo an operator rightsizing change. The change id (rs-xxxxxxxx) is in
the rightsize.apply audit event, the RightsizeApplied Kubernetes event on
the workload, and the policy's status.plannedChanges.

undo finds the workload carrying the change in its
kubehero.io/rightsize-previous history, restores the requests/limits in
place before that change (reverting any later ones too), and marks the
workload kubehero.io/rightsize-reverted so the operator leaves it alone
until someone removes that annotation. It runs kubectl with your
kubeconfig and RBAC, and refuses when the resources were changed by
someone else since KubeHero's last change (override with --force).`,
		Example: `  kubehero undo rs-5f2c9a1e
  kubehero undo rs-5f2c9a1e --namespace payments --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner := kube.Kubectl{Kubeconfig: kubeconfig, Context: kubeContext}
			return runUndo(cmd, runner, args[0], namespace, force, dryRun, time.Now())
		},
	}
	f := c.Flags()
	f.StringVarP(&namespace, "namespace", "n", "", "Namespace to search (default: all namespaces)")
	f.StringVar(&kubeconfig, "kubeconfig", "", "Path to the kubeconfig file")
	f.StringVar(&kubeContext, "context", "", "kubeconfig context")
	f.BoolVar(&force, "force", false, "Restore even if the resources changed since KubeHero's last change")
	f.BoolVar(&dryRun, "dry-run", false, "Print the patch without applying it")
	return c
}

func runUndo(cmd *cobra.Command, runner kube.Runner, id, namespace string, force, dryRun bool, now time.Time) error {
	id = strings.TrimSpace(id)
	if !changeIDRE.MatchString(id) {
		if strings.HasPrefix(id, "aud-") || strings.Trim(id, "0123456789") == "" {
			return fmt.Errorf("%q looks like an audit-log id; undo takes the rightsizing change id (rs-xxxxxxxx) "+
				"recorded in that audit event's payload and in the workload's RightsizeApplied event", id)
		}
		return fmt.Errorf("%q is not a rightsizing change id (rs-xxxxxxxx)", id)
	}
	ctx, cancel := context.WithTimeout(cmdContext(cmd), 2*time.Minute)
	defer cancel()

	scope := []string{"--all-namespaces"}
	if namespace != "" {
		scope = []string{"--namespace", namespace}
	}
	raw, err := runner.Run(ctx, append([]string{"get", "deployments.apps,statefulsets.apps", "-o", "json"}, scope...)...)
	if err != nil {
		return err
	}
	var list struct {
		Items []workloadObj `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("parse kubectl output: %w", err)
	}
	obj, hist, idx, err := findChange(list.Items, id)
	if err != nil {
		return err
	}

	target, drift := restorePlan(obj, hist, idx)
	if len(drift) > 0 && !force {
		return fmt.Errorf("%s/%s was modified after KubeHero's last change (%s); re-run with --force to restore anyway",
			obj.Metadata.Namespace, obj.Metadata.Name, strings.Join(drift, "; "))
	}

	remaining := rightsizeHistory{Version: 1, Changes: hist.Changes[idx+1:]}
	annotations := map[string]any{annRightsizeReverted: now.UTC().Format(time.RFC3339)}
	if len(remaining.Changes) == 0 {
		annotations[annRightsizePrevious] = nil
	} else {
		b, _ := json.Marshal(remaining)
		annotations[annRightsizePrevious] = string(b)
	}
	containers := make([]map[string]any, 0, len(target))
	names := make([]string, 0, len(target))
	for n := range target {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		res := map[string]any{}
		for section, vals := range target[n] {
			m := map[string]any{}
			for k, v := range vals {
				if v == "" {
					m[k] = nil // the key didn't exist before: remove it
				} else {
					m[k] = v
				}
			}
			res[section] = m
		}
		containers = append(containers, map[string]any{"name": n, "resources": res})
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": obj.Metadata.ResourceVersion, "annotations": annotations},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"containers": containers}}},
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	ref := strings.ToLower(obj.Kind) + "/" + obj.Metadata.Name
	if dryRun {
		fmt.Fprintf(out, "would patch %s -n %s (restoring the state before %s):\n%s\n", ref, obj.Metadata.Namespace, id, patch)
		return nil
	}
	if _, err := runner.Run(ctx, "patch", ref, "--namespace", obj.Metadata.Namespace,
		"--type=strategic", "--field-manager="+undoFieldManager, "-p", string(patch)); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ restored %s/%s to its resources before %s (%d later change(s) also reverted)\n",
		obj.Metadata.Namespace, obj.Metadata.Name, id, idx)
	for _, n := range names {
		fmt.Fprintf(out, "  %s: %s\n", n, describeValues(target[n]))
	}
	fmt.Fprintf(out, "  marked %s — the operator won't rightsize it again until that annotation is removed\n", annRightsizeReverted)
	auditUndo(cmd.ErrOrStderr(), obj, id, target)
	return nil
}

// findChange locates the workload whose history holds change id.
func findChange(items []workloadObj, id string) (workloadObj, rightsizeHistory, int, error) {
	for _, it := range items {
		raw := it.Metadata.Annotations[annRightsizePrevious]
		if raw == "" || !strings.Contains(raw, id) {
			continue
		}
		var h rightsizeHistory
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			return it, h, 0, fmt.Errorf("%s/%s: unreadable %s annotation: %w", it.Metadata.Namespace, it.Metadata.Name, annRightsizePrevious, err)
		}
		if h.Version != 1 {
			return it, h, 0, fmt.Errorf("%s/%s: rightsize history version %d is newer than this CLI understands; upgrade kubehero",
				it.Metadata.Namespace, it.Metadata.Name, h.Version)
		}
		for i, c := range h.Changes {
			if c.ChangeID == id {
				return it, h, i, nil
			}
		}
	}
	return workloadObj{}, rightsizeHistory{}, 0, fmt.Errorf("no Deployment or StatefulSet carries change %s "+
		"(it may be older than the last 5 changes, already undone, or in a namespace you can't read)", id)
}

// restorePlan computes, per container, the value every touched key had
// before change idx (changes newer than idx are unwound too: the oldest
// change touching a key decides its restored value), and reports drift —
// keys whose current value no longer matches KubeHero's latest change.
func restorePlan(obj workloadObj, h rightsizeHistory, idx int) (map[string]map[string]map[string]string, []string) {
	target := map[string]map[string]map[string]string{}
	for j := 0; j <= idx; j++ {
		for _, c := range h.Changes[j].Containers {
			if target[c.Name] == nil {
				target[c.Name] = map[string]map[string]string{}
			}
			for section, applied := range map[string]map[string]string{"requests": c.Applied.Requests, "limits": c.Applied.Limits} {
				prev := c.Previous.Requests
				if section == "limits" {
					prev = c.Previous.Limits
				}
				for k := range applied {
					if target[c.Name][section] == nil {
						target[c.Name][section] = map[string]string{}
					}
					target[c.Name][section][k] = prev[k]
				}
			}
		}
	}
	current := map[string]resourceValues{}
	for _, c := range obj.Spec.Template.Spec.Containers {
		current[c.Name] = c.Resources
	}
	var drift []string
	for _, c := range h.Changes[0].Containers {
		cur := current[c.Name]
		check := func(section string, applied, now map[string]string) {
			for k, want := range applied {
				if got := now[k]; !sameQuantity(got, want) {
					drift = append(drift, fmt.Sprintf("%s %s.%s is %s, KubeHero set %s", c.Name, section, k, nonEmptyStr(got, "unset"), want))
				}
			}
		}
		check("requests", c.Applied.Requests, cur.Requests)
		check("limits", c.Applied.Limits, cur.Limits)
	}
	sort.Strings(drift)
	return target, drift
}

// sameQuantity compares Kubernetes quantities loosely ("1000m" == "1").
func sameQuantity(a, b string) bool {
	if a == b {
		return true
	}
	na, oka := parseQuantity(a)
	nb, okb := parseQuantity(b)
	return oka && okb && na == nb
}

var quantitySuffix = map[string]float64{
	"m": 1e-3, "": 1, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
}

func parseQuantity(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') {
		i--
	}
	mult, ok := quantitySuffix[s[i:]]
	if !ok || i == 0 {
		return 0, false
	}
	var v float64
	if _, err := fmt.Sscanf(s[:i], "%g", &v); err != nil {
		return 0, false
	}
	return v * mult, true
}

func describeValues(v map[string]map[string]string) string {
	var parts []string
	for _, section := range []string{"requests", "limits"} {
		keys := make([]string, 0, len(v[section]))
		for k := range v[section] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s.%s=%s", section, k, nonEmptyStr(v[section][k], "(removed)")))
		}
	}
	return strings.Join(parts, " ")
}

// auditUndo records the revert in the control plane's audit log when one
// is configured. Best effort: the cluster change already happened.
func auditUndo(errOut io.Writer, obj workloadObj, id string, target map[string]map[string]map[string]string) {
	cfg := resolveConfig()
	if cfg.Endpoint == "" {
		fmt.Fprintln(errOut, "note: no control plane configured; the undo was not recorded in the audit log")
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"changeId": id, "namespace": obj.Metadata.Namespace, "workload": obj.Metadata.Name,
		"kind": obj.Kind, "restored": target,
	})
	err := client.New(cfg).AppendAuditEntry(&client.AppendAuditEntryRequest{
		Action: "rightsize.undo", TargetKind: obj.Kind, TargetName: obj.Metadata.Namespace + "/" + obj.Metadata.Name,
		ActorSub: "kubehero-cli", Outcome: "reverted", Payload: payload,
	})
	if err != nil {
		fmt.Fprintf(errOut, "warning: undo applied but the audit entry failed: %v\n", err)
		return
	}
	fmt.Fprintln(errOut, "recorded rightsize.undo in the audit log")
}
