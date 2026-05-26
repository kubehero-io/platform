// SPDX-License-Identifier: Apache-2.0
package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpMentionsSubcommands(t *testing.T) {
	buf := &bytes.Buffer{}
	r := Root()
	r.SetOut(buf)
	r.SetArgs([]string{"--help"})
	if err := r.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"scan", "rightsize", "apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\n%s", want, out)
		}
	}
}
