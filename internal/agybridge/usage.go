package agybridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
)

const usageTimeout = 45 * time.Second

// usageFresh is how long a plan-usage reading is reused. Each read starts a
// whole CLI process, so repeated quota refreshes share one.
const usageFresh = 30 * time.Second

// PlanBucket is one plan limit as the CLI reports it: a group of models that
// share a limit, and how much of it is used.
type PlanBucket struct {
	Group    string
	Window   string // "weekly" on the plans measured; other words are passed on
	Used     float64
	ResetsAt string
}

// PlanUsageReader is the optional half of an Executor that can report plan usage.
type PlanUsageReader interface {
	PlanUsage(ctx context.Context) ([]PlanBucket, error)
}

// SupportsPlanUsage reports whether `agy -p /usage` is answered by the CLI. From
// 1.1.11 print mode answers it without a turn; an older CLI would send the words
// to a model and spend quota, so an unreadable version is a no.
func SupportsPlanUsage(version string) bool {
	want := [3]int{1, 1, 11}
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) != 3 {
		return false
	}
	var have [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		have[i] = n
	}
	return slices.Compare(have[:], want[:]) >= 0
}

// PlanUsage reads the plan limits with `agy -p /usage`. It starts the CLI once
// and runs no turn: the reply must say it answered the usage command.
//
// Reads run one at a time and a success is reused for usageFresh: without
// that, every quota refresh started its own CLI process (about 200 MB each)
// next to the turns in flight.
func (r *Runner) PlanUsage(ctx context.Context) ([]PlanBucket, error) {
	r.usageMu.Lock()
	defer r.usageMu.Unlock()
	if r.usageLast != nil && time.Since(r.usageAt) < usageFresh {
		return slices.Clone(r.usageLast), nil
	}
	out, err := r.readPlanUsage(ctx)
	if err == nil {
		r.usageLast, r.usageAt = slices.Clone(out), time.Now()
		if r.usageLast == nil {
			r.usageLast = []PlanBucket{}
		}
	}
	return out, err
}

func (r *Runner) readPlanUsage(ctx context.Context) ([]PlanBucket, error) {
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	work, err := os.MkdirTemp("", "agy-bridge-")
	if err != nil {
		return nil, fmt.Errorf("agy-bridge: cannot create work dir: %w", err)
	}
	defer os.RemoveAll(work)

	cmd := exec.CommandContext(ctx, r.Bin, "--print=/usage", "--output-format", "json")
	cmd.Dir = work
	cmd.Env = childEnv(r.Home)
	bridgeutil.SetProcessGroup(cmd)
	cmd.Cancel = func() error { bridgeutil.KillGroup(cmd); return nil }
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, deadlineErr(ctx.Err())
	}
	if runErr != nil {
		return nil, mapError("ERROR", strings.TrimSpace(stderr.String()))
	}
	return ParsePlanUsage(stdout.Bytes())
}

// ParsePlanUsage reads the JSON reply to /usage (shape measured on 1.5.13). A
// bucket without a remaining fraction is unknown, not empty, and is left out.
func ParsePlanUsage(body []byte) ([]PlanBucket, error) {
	var reply struct {
		Status   string `json:"status"`
		Error    string `json:"error"`
		NumTurns int    `json:"num_turns"`
		Command  struct {
			Name string `json:"name"`
			Data struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						Window            string   `json:"window"`
						RemainingFraction *float64 `json:"remaining_fraction"`
						ResetTime         string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, &NativeError{Status: 502, Type: "api_error", Code: "native_error", Message: "agy-bridge: the CLI's usage reply is not JSON"}
	}
	if reply.Status != "SUCCESS" {
		return nil, mapError(reply.Status, reply.Error)
	}
	if reply.Command.Name != "usage" || reply.NumTurns != 0 {
		return nil, &NativeError{Status: 502, Type: "api_error", Code: "native_error", Message: "agy-bridge: the CLI did not answer the usage command"}
	}
	var out []PlanBucket
	for _, g := range reply.Command.Data.Groups {
		for _, b := range g.Buckets {
			if b.RemainingFraction == nil {
				continue
			}
			used := math.Round((1-*b.RemainingFraction)*10000) / 100
			out = append(out, PlanBucket{Group: g.Name, Window: b.Window, Used: min(100, max(0, used)), ResetsAt: b.ResetTime})
		}
	}
	return out, nil
}

// usageBody renders the reading in the percent quota shape the provider cards
// read (provider.ClassifyQuota): per window, the fullest group; plus every group
// by name, since a full group does not stop the others' models.
func usageBody(buckets []PlanBucket, now time.Time) ([]byte, bool) {
	usage, groups := map[string]any{}, []map[string]any{}
	worst := map[string]float64{}
	perGroup := map[string]int{}
	for _, b := range buckets {
		perGroup[b.Group]++
	}
	for _, b := range buckets {
		shape := b.Window
		if shape != "weekly" && shape != "monthly" {
			shape = "rolling"
		}
		if have, seen := worst[shape]; !seen || b.Used > have {
			worst[shape] = b.Used
			entry := map[string]any{"percent": b.Used}
			if b.ResetsAt != "" {
				entry["resetsAt"] = b.ResetsAt
			}
			usage[shape] = entry
		}
		name := b.Group
		if perGroup[b.Group] > 1 && b.Window != "" {
			name += " (" + b.Window + ")"
		}
		group := map[string]any{"name": name, "window": b.Window, "percent": b.Used}
		if b.ResetsAt != "" {
			group["resets_at"] = b.ResetsAt
		}
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return nil, false
	}
	body, err := json.Marshal(map[string]any{
		"usage": usage, "groups": groups,
		"source": "agy -p /usage", "observed_at": now.UTC().Format(time.RFC3339),
	})
	return body, err == nil
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.Runner.(PlanUsageReader)
	if !ok || !s.PlanUsage {
		bridgeutil.WriteJSONBytes(w, http.StatusNotFound, bridgeutil.OpenAIErrorEnvelope("not_found_error",
			"agy-bridge: this CLI does not report plan usage", "no_reading"))
		return
	}
	buckets, err := reader.PlanUsage(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	body, ok := usageBody(buckets, s.now())
	if !ok {
		bridgeutil.WriteJSONBytes(w, http.StatusNotFound, bridgeutil.OpenAIErrorEnvelope("not_found_error",
			"agy-bridge: the login reported no plan limit", "no_reading"))
		return
	}
	bridgeutil.WriteJSONBytes(w, http.StatusOK, body)
}
