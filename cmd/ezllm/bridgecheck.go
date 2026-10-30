package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/agybridge"
	"github.com/c4lyp5o/ezllm/internal/codexbridge"
)

// Bridge CLI qualification and the bridge-check gate (SPEC-bridge-update.md
// §4.2–4.3). The host updater runs `ezllm bridge-check` against a staged CLI
// before it promotes it; boot runs the same checks, so the updater cannot pass
// a release boot would reject. The gate is stricter than boot: flag drift, the
// exec fallback or a lost code-mode host are reported and served by boot, but
// refuse a promotion.

// qualifiedFile is the updater's record of versions that passed the live proof.
// It sits in the bridge directory, next to the version folders and `current`.
const qualifiedFile = "qualified.json"

type qualifiedRecord struct {
	Versions map[string]json.RawMessage `json:"versions"`
}

// recordQualifies reports whether version is listed in the qualified.json
// nearest the binary: its own directory, then up to two parents — the bridge
// root for both `current/claude` and `current/bin/codex`. The walk is lexical,
// so a `current` symlink leads to the bridge root, not into the version folder.
func recordQualifies(bin, version string) (bool, error) {
	dir := filepath.Dir(bin)
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, qualifiedFile)
		b, err := os.ReadFile(path)
		if err == nil {
			var rec qualifiedRecord
			if err := json.Unmarshal(b, &rec); err != nil {
				return false, fmt.Errorf("%s: %w", path, err)
			}
			_, ok := rec.Versions[version]
			return ok, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return false, nil
}

// releaseState is the `cli_release` a bridge reports: qualified when the
// version is the one this build was measured on or the updater proved it.
func releaseState(bin, version, measured string) string {
	if version == measured {
		return "qualified"
	}
	ok, err := recordQualifies(bin, version)
	switch {
	case err != nil:
		return "unqualified (" + qualifiedFile + " unreadable: " + err.Error() + ")"
	case ok:
		return "qualified (live proof recorded by the updater)"
	}
	return "unqualified (measured on " + measured + "; re-run the live proof)"
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// bridgeVerdict is what `ezllm bridge-check --json` prints for the updater.
type bridgeVerdict struct {
	Bridge   string   `json:"bridge"`
	OK       bool     `json:"ok"`
	Version  string   `json:"version,omitempty"`
	Release  string   `json:"release,omitempty"`
	Problems []string `json:"problems"`
}

var errBridgeCheckFailed = errors.New("bridge-check: the CLI did not pass")

func runBridgeCheck(args []string, stdout io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ezllm bridge-check {claude|codex|agy} [--bin PATH] [--home PATH] [--json]")
	}
	name := args[0]
	set := flag.NewFlagSet("bridge-check "+name, flag.ContinueOnError)
	bin := set.String("bin", "", "CLI binary (default: the bridge's EZLLM_*_BIN or its default)")
	home := set.String("home", "", "login directory (default: the bridge's login env or its default)")
	asJSON := set.Bool("json", false, "print the verdict as JSON")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}

	ctx := context.Background()
	var v bridgeVerdict
	switch name {
	case "claude":
		v = checkClaudeCLI(or(*bin, envOrDefault(envBridgeBin, "/opt/claude/claude")), or(*home, envOrDefault(envBridgeDir, "/claude-sub")))
	case "codex":
		v = checkCodexCLI(ctx, or(*bin, envOrDefault(envCodexBin, "/opt/codex/bin/codex")), or(*home, envOrDefault(envCodexHome, "/codex-sub")))
	case "agy":
		v = checkAgyCLI(ctx, or(*bin, envOrDefault(envAgyBin, "/opt/agy/agy")), or(*home, envOrDefault(envAgyHome, "/agy-sub")))
	default:
		return fmt.Errorf("bridge-check: unknown bridge %q (claude, codex or agy)", name)
	}
	v.OK = len(v.Problems) == 0
	if v.Problems == nil {
		v.Problems = []string{}
	}
	if *asJSON {
		b, _ := json.Marshal(v)
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintf(stdout, "%s %s: %s\n", v.Bridge, v.Version, v.Release)
		for _, p := range v.Problems {
			fmt.Fprintln(stdout, "  problem:", p)
		}
	}
	if !v.OK {
		return errBridgeCheckFailed
	}
	return nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func checkClaudeCLI(bin, dir string) bridgeVerdict {
	v := bridgeVerdict{Bridge: "claude"}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		v.Problems = append(v.Problems, "binary does not resolve: "+err.Error())
		return v
	}
	raw, drift := claudeCLICheck(resolved)
	if raw == "unavailable" {
		v.Problems = append(v.Problems, drift)
		return v
	}
	v.Version = claudeVersion(raw)
	v.Release = releaseState(bin, v.Version, claudeQualifiedVersion)
	if drift != "" {
		v.Problems = append(v.Problems, "flags missing from --help: "+drift)
	}
	if state, plan := claudeLoginCheck(resolved, dir); state != "logged-in" {
		v.Problems = append(v.Problems, "login "+state+": "+plan)
	}
	return v
}

func checkCodexCLI(ctx context.Context, bin, home string) bridgeVerdict {
	v := bridgeVerdict{Bridge: "codex"}
	if runtime.GOOS != "linux" {
		v.Problems = append(v.Problems, "the codex bridge is a Linux feature (this build is "+runtime.GOOS+")")
		return v
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		v.Problems = append(v.Problems, "binary does not resolve: "+err.Error())
		return v
	}
	pre, err := codexbridge.RunPreflight(ctx, resolved, home)
	v.Version = pre.Version
	if pre.Version != "" {
		v.Release = releaseState(bin, pre.Version, codexbridge.QualifiedVersion)
	}
	if err != nil {
		v.Problems = append(v.Problems, err.Error())
		return v
	}
	if pre.Channel == codexbridge.ChannelExec {
		v.Problems = append(v.Problems, "app-server lacks "+strings.Join(pre.MissingRPCs, ", ")+": the bridge would fall back to exec")
	}
	if host := codexbridge.CodeModeHost(resolved); !fileExists(host) {
		v.Problems = append(v.Problems, "code-mode host missing ("+host+"): client tools would be off")
	}
	return v
}

func checkAgyCLI(ctx context.Context, bin, home string) bridgeVerdict {
	v := bridgeVerdict{Bridge: "agy"}
	if runtime.GOOS != "linux" {
		v.Problems = append(v.Problems, "the agy bridge is a Linux feature (this build is "+runtime.GOOS+")")
		return v
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		v.Problems = append(v.Problems, "binary does not resolve: "+err.Error())
		return v
	}
	pre, err := agybridge.RunPreflight(ctx, resolved, home)
	v.Version = pre.Version
	if pre.Version != "" {
		v.Release = releaseState(bin, pre.Version, agybridge.QualifiedVersion)
	}
	if err != nil {
		v.Problems = append(v.Problems, err.Error())
		return v
	}
	if len(pre.Models) == 0 {
		v.Problems = append(v.Problems, "the CLI listed no models")
	}
	return v
}
