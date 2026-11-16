package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// TestPrintStatusVersions checks version reporting and actionable warnings without
// mistaking older clients, legacy daemons or development builds for stale daemons.
func TestPrintStatusVersions(t *testing.T) {
	for _, tt := range []struct {
		name           string
		client, daemon string
		warn           bool
	}{
		{name: "same release", client: "v0.6.0", daemon: "v0.6.0"},
		{name: "older daemon", client: "v0.6.1", daemon: "v0.6.0", warn: true},
		{name: "older client", client: "v0.6.0", daemon: "v0.6.1", warn: true},
		{name: "legacy daemon", client: "v0.6.1"},
		{name: "development daemon", client: "v0.6.1", daemon: "(devel)"},
		{name: "development client", client: "(devel)", daemon: "v0.6.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := api.Status{BuildVersion: tt.daemon, Proto: "nfs", Port: 2049}
			var out, warnings bytes.Buffer
			printStatus(&out, &warnings, &st, tt.client)
			daemon := tt.daemon
			if daemon == "" {
				daemon = "unknown"
			}
			if !hasRow(out.String(), "daemon", daemon) || !hasRow(out.String(), "client", tt.client) {
				t.Fatalf("missing process versions:\n%s", &out)
			}
			if strings.Contains(out.String(), "ovnfs restart") {
				t.Fatalf("warning mixed into status output:\n%s", &out)
			}
			if !tt.warn {
				if warnings.Len() != 0 {
					t.Fatalf("unwarranted version warning: %s", &warnings)
				}
				return
			}
			for _, detail := range []string{tt.client, tt.daemon, "ovnfs restart", "--state", "--mount"} {
				if !strings.Contains(warnings.String(), detail) {
					t.Errorf("warning omits %q: %s", detail, &warnings)
				}
			}
		})
	}
}
