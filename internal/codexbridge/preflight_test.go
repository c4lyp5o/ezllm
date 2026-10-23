package codexbridge

import (
	"context"
	"strings"
	"testing"
)

func TestLockdownViolations(t *testing.T) {
	var b strings.Builder
	for _, f := range LockdownFeatures {
		b.WriteString(f + "                     stable             false\n")
	}
	if bad := lockdownViolations(b.String()); len(bad) != 0 {
		t.Fatalf("all-false list reported %v", bad)
	}
	list := strings.Replace(b.String(), "shell_tool                     stable             false", "shell_tool   stable   true", 1)
	list = strings.Replace(list, "code_mode_host", "renamed_host", 1)
	bad := lockdownViolations(list)
	if strings.Join(bad, ",") != "shell_tool=true,code_mode_host=missing" {
		t.Fatalf("violations = %v, want shell_tool=true and code_mode_host=missing", bad)
	}
	// unified_exec ignores its --disable on 0.163.0; reported true, it is not a
	// violation — but a release that drops the feature name still is.
	list = strings.Replace(b.String(), "unified_exec                     stable             false", "unified_exec   stable   true", 1)
	if bad := lockdownViolations(list); len(bad) != 0 {
		t.Fatalf("unified_exec=true reported %v", bad)
	}
	list = strings.Replace(b.String(), "unified_exec                     stable             false\n", "", 1)
	if bad := lockdownViolations(list); strings.Join(bad, ",") != "unified_exec=missing" {
		t.Fatalf("a dropped unified_exec = %v, want unified_exec=missing", bad)
	}
}

func TestMissingNames(t *testing.T) {
	schema := `{"enum":["initialize"]} {"enum":["thread/start"]} {"enum":["model/list"]}`
	got := missingNames(schema, RequiredMethods)
	if strings.Join(got, ",") != "turn/start,account/rateLimits/read" {
		t.Fatalf("missing = %v", got)
	}
}

// The real CLI, when present on this host: the lockdown and schema checks
// must pass against the qualified release.
func TestPreflightAgainstTheInstalledCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns the real CLI")
	}
	bin, home := liveCLI(t)
	p, err := RunPreflight(context.Background(), bin, home)
	if err != nil {
		t.Fatalf("preflight: %v (%+v)", err, p)
	}
	if !p.LockdownOK || !p.SchemaOK || p.Version == "" {
		t.Fatalf("preflight = %+v", p)
	}
}
