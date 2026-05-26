// SPDX-License-Identifier: BUSL-1.1
package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
)

func boolPtr(b bool) *bool { return &b }

func TestIsArmed(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		humanArm    *bool
		want        bool
	}{
		{"nil humanArm defaults true, no annotation → locked", nil, nil, false},
		{"humanArm=true, no annotation → locked", nil, boolPtr(true), false},
		{"humanArm=true, annotation=true → armed", map[string]string{AnnotationArmed: "true"}, boolPtr(true), true},
		{"humanArm=true, annotation=false → locked", map[string]string{AnnotationArmed: "false"}, boolPtr(true), false},
		{"humanArm=false → always armed", nil, boolPtr(false), true},
	}
	for _, c := range cases {
		obj := &kubeherov1.BudgetPolicy{
			ObjectMeta: metav1.ObjectMeta{Annotations: c.annotations},
		}
		if got := IsArmed(obj, c.humanArm); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
