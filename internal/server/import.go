package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// maxImportBytes bounds an import upload. A configuration export is small; a
// file this large almost always carries the ledger, which import ignores.
const maxImportBytes = 32 << 20

// importDoc is the part of a configuration export (GET /admin/export) that
// import reads. Tokens, prices, catalogs, quotas and the ledger are ignored:
// tokens have no secret in the file, and the rest is refreshed or measured.
type importDoc struct {
	FormatVersion int                        `json:"format_version"`
	Kind          string                     `json:"kind"`
	Settings      map[string]json.RawMessage `json:"settings"`
	Accounts      []store.AccountSummary     `json:"accounts"`
	Compression   []store.CompressionProfile `json:"compression_profiles"`
	Combos        []store.Combo              `json:"combos"`
	Rules         []store.ModelRule          `json:"model_rules"`
}

// ImportItem is one line of an import plan.
type ImportItem struct {
	Section string   `json:"section"` // settings | accounts | compression_profiles | combos | model_rules
	Name    string   `json:"name"`
	Action  string   `json:"action"` // create | update | unchanged | skip
	Changes []string `json:"changes,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// importPlan is what an import will do, worked out against this instance
// before anything is written.
type importPlan struct {
	items    []ImportItem
	settings map[string]json.RawMessage
	accounts []plannedAccount
	profiles []store.CompressionProfile // ID 0 = create
	combos   []plannedCombo
	rules    []plannedRule
}

type plannedAccount struct {
	id int64 // 0 = create
	in store.AccountInput
}

// plannedCombo and plannedRule name accounts by namespace and profiles by
// name: an account the import creates has no id until it is written.
type plannedCombo struct {
	combo    store.Combo
	hopNS    []string
	freeNS   []string
	inclNS   []string
	profile  string
	existing int64
}

type plannedRule struct {
	rule store.ModelRule
	ns   string
}

// handleImport reads a configuration export and applies it: POST
// /admin/import?dry_run=true answers the plan only. An account that exists
// here (same namespace) is updated and keeps its keys; a new account is
// created turned off, because the file has no keys. Combos, compression
// profiles and model rules match by name, and by account and model.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	dryRun := r.URL.Query().Get("dry_run") == "true"
	var doc importDoc
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportBytes))
	if err := dec.Decode(&doc); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "the file is over 32 MB: export without the ledger and try again")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "not a configuration export: "+err.Error())
		return
	}
	if doc.Kind != "configuration_export" || doc.FormatVersion != 1 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "not a configuration export (kind configuration_export, format_version 1)")
		return
	}
	plan, err := s.planImport(r.Context(), doc)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !dryRun {
		if err := s.applyImport(r.Context(), plan); err != nil {
			s.log.Warn("import stopped part way", "err", err)
			writeErrExtra(w, http.StatusConflict, "conflict", "import stopped part way: "+err.Error()+". What was written stays; fix the cause and import again.", nil)
			return
		}
		s.log.Info("configuration imported", "items", len(plan.items))
	}
	summary := map[string]int{}
	for _, it := range plan.items {
		summary[it.Action]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"dry_run": dryRun, "items": plan.items, "summary": summary})
}

func (s *Server) planImport(ctx context.Context, doc importDoc) (*importPlan, error) {
	p := &importPlan{items: []ImportItem{}}
	add := func(section, name, action, note string, changes []string) {
		p.items = append(p.items, ImportItem{Section: section, Name: name, Action: action, Changes: changes, Note: note})
	}

	// Settings: only keys this version knows and the host does not pin.
	if len(doc.Settings) > 0 && s.settings != nil {
		view := s.settings.View()
		p.settings = map[string]json.RawMessage{}
		for k, raw := range doc.Settings {
			f, known := view.Fields[k]
			switch {
			case !known:
				add("settings", k, "skip", "this version has no such setting", nil)
			case !f.Editable:
				add("settings", k, "skip", "set by "+f.Source+" on this host", nil)
			case jsonEqual(raw, f.Value):
				add("settings", k, "unchanged", "", nil)
			default:
				p.settings[k] = raw
				add("settings", k, "update", "", nil)
			}
		}
		if err := s.settings.Validate(p.settings); err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}
	}

	// Accounts, by namespace.
	current, err := s.db.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	byNS := map[string]store.AccountSummary{}
	nameOwner := map[string]string{}
	for _, a := range current {
		byNS[a.Namespace] = a
		nameOwner[a.Name] = a.Namespace
	}
	srcNS := map[int64]string{} // the export's account ids → namespace
	known := map[string]bool{}  // namespaces that will exist after the import
	for ns := range byNS {
		known[ns] = true
	}
	for _, a := range doc.Accounts {
		srcNS[a.ID] = a.Namespace
		in := accountInputFrom(a)
		if owner, taken := nameOwner[a.Name]; taken && owner != a.Namespace {
			add("accounts", a.Namespace, "skip", fmt.Sprintf("the name %q belongs to %s here", a.Name, owner), nil)
			continue
		}
		cur, exists := byNS[a.Namespace]
		if !exists {
			off := false
			in.Enabled = &off
			if err := store.ValidateAccountInput(in); err != nil {
				return nil, fmt.Errorf("account %s: %w", a.Namespace, err)
			}
			p.accounts = append(p.accounts, plannedAccount{in: in})
			known[a.Namespace] = true
			nameOwner[a.Name] = a.Namespace
			add("accounts", a.Namespace, "create", "created turned off: add a key, then turn it on", nil)
			continue
		}
		if err := store.ValidateAccountInput(in); err != nil {
			return nil, fmt.Errorf("account %s: %w", a.Namespace, err)
		}
		note := "keeps its keys"
		if a.Enabled && !cur.Enabled && !hasEnabledKey(cur) {
			// The file has no keys: an account with none stays off.
			off := false
			in.Enabled, a.Enabled = &off, false
			note = "stays off until it has a key"
		}
		changes := accountChanges(cur, a)
		if len(changes) == 0 {
			add("accounts", a.Namespace, "unchanged", "", nil)
			continue
		}
		p.accounts = append(p.accounts, plannedAccount{id: cur.ID, in: in})
		add("accounts", a.Namespace, "update", note, changes)
	}

	// Compression profiles, by name.
	profiles, err := s.db.ListCompressionProfiles(ctx)
	if err != nil {
		return nil, err
	}
	profByName := map[string]store.CompressionProfile{}
	for _, pr := range profiles {
		profByName[pr.Name] = pr
	}
	srcProfile := map[int64]string{}
	knownProfiles := map[string]bool{}
	for n := range profByName {
		knownProfiles[n] = true
	}
	for _, pr := range doc.Compression {
		srcProfile[pr.ID] = pr.Name
		knownProfiles[pr.Name] = true
		cur, exists := profByName[pr.Name]
		next := pr
		next.ID, next.CreatedAt = 0, ""
		if !exists {
			p.profiles = append(p.profiles, next)
			add("compression_profiles", pr.Name, "create", "", nil)
			continue
		}
		next.ID = cur.ID
		if changes := profileChanges(cur, pr); len(changes) > 0 {
			p.profiles = append(p.profiles, next)
			add("compression_profiles", pr.Name, "update", "", changes)
		} else {
			add("compression_profiles", pr.Name, "unchanged", "", nil)
		}
	}

	// Combos, by name. Hops on an account neither here nor in the file are
	// dropped and named.
	combos, err := s.db.ListCombos(ctx)
	if err != nil {
		return nil, err
	}
	comboByName := map[string]store.Combo{}
	nsByID := map[int64]string{}
	for _, a := range current {
		nsByID[a.ID] = a.Namespace
	}
	for _, c := range combos {
		comboByName[c.Name] = c
	}
	for _, c := range doc.Combos {
		pc := plannedCombo{combo: c}
		var dropped []string
		var hops []store.ComboHop
		for _, h := range c.Hops {
			ns := srcNS[h.AccountID]
			if ns == "" || !known[ns] {
				dropped = append(dropped, fmt.Sprintf("%s/%s", orUnknown(ns), h.ModelID))
				continue
			}
			hops = append(hops, h)
			pc.hopNS = append(pc.hopNS, ns)
		}
		pc.combo.Hops = hops
		for _, id := range c.AutoFreeAccounts {
			if ns := srcNS[id]; ns != "" && known[ns] {
				pc.freeNS = append(pc.freeNS, ns)
			}
		}
		var incl []store.FreeInclude
		for _, fi := range c.AutoFreeInclude {
			if ns := srcNS[fi.AccountID]; ns != "" && known[ns] {
				incl = append(incl, fi)
				pc.inclNS = append(pc.inclNS, ns)
			}
		}
		pc.combo.AutoFreeInclude = incl
		if c.CompressionProfileID != nil {
			pc.profile = srcProfile[*c.CompressionProfileID]
			if !knownProfiles[pc.profile] {
				pc.profile = ""
			}
		}
		check := pc.combo
		check.CompressionProfileID = nil
		if check.ContextSize == 0 {
			check.ContextSize = 200000
		}
		if check.BlockSize == 0 {
			check.BlockSize = 1
		}
		if err := store.ValidateCombo(check); err != nil {
			return nil, fmt.Errorf("combo %s: %w", c.Name, err)
		}
		note := ""
		if len(dropped) > 0 {
			note = "hops left out (account not here or in the file): " + strings.Join(dropped, ", ")
		}
		cur, exists := comboByName[c.Name]
		if !exists {
			p.combos = append(p.combos, pc)
			add("combos", c.Name, "create", note, nil)
			continue
		}
		pc.existing = cur.ID
		curProfile := ""
		if cur.CompressionProfileID != nil {
			for _, pr := range profiles {
				if pr.ID == *cur.CompressionProfileID {
					curProfile = pr.Name
				}
			}
		}
		if changes := comboChanges(cur, nsByID, curProfile, pc); len(changes) > 0 {
			p.combos = append(p.combos, pc)
			add("combos", c.Name, "update", note, changes)
		} else {
			add("combos", c.Name, "unchanged", note, nil)
		}
	}

	// Model rules, by account and model.
	curRules, err := s.db.ListModelRules(ctx, 0)
	if err != nil {
		return nil, err
	}
	ruleAt := map[string]store.ModelRule{}
	for _, r := range curRules {
		ruleAt[nsByID[r.AccountID]+"/"+r.ModelID] = r
	}
	for _, r := range doc.Rules {
		ns := srcNS[r.AccountID]
		name := orUnknown(ns) + "/" + r.ModelID
		if ns == "" || !known[ns] {
			add("model_rules", name, "skip", "its account is not here or in the file", nil)
			continue
		}
		check := r
		if err := store.ValidateModelRule(&check); err != nil {
			return nil, fmt.Errorf("rule %s: %w", name, err)
		}
		cur, exists := ruleAt[name]
		switch {
		case !exists:
			p.rules = append(p.rules, plannedRule{rule: r, ns: ns})
			add("model_rules", name, "create", "", nil)
		case ruleChanged(cur, r):
			p.rules = append(p.rules, plannedRule{rule: r, ns: ns})
			add("model_rules", name, "update", "", nil)
		default:
			add("model_rules", name, "unchanged", "", nil)
		}
	}
	return p, nil
}

// applyImport writes a plan: settings first (refused as a whole when
// invalid, before anything else is written), then accounts, profiles, combos
// and rules. Every step matches by name, so running an import again after a
// failure finishes the job.
func (s *Server) applyImport(ctx context.Context, p *importPlan) error {
	if len(p.settings) > 0 {
		if _, err := s.settings.Patch(ctx, s.settings.View().Revision, p.settings); err != nil {
			return fmt.Errorf("settings: %w", err)
		}
	}
	for _, a := range p.accounts {
		var err error
		if a.id == 0 {
			_, err = s.db.CreateAccount(ctx, a.in)
		} else {
			err = s.db.UpdateAccount(ctx, a.id, a.in)
		}
		if err != nil {
			return fmt.Errorf("account %s: %w", a.in.Namespace, err)
		}
	}
	for _, pr := range p.profiles {
		if _, err := s.db.UpsertCompressionProfile(ctx, pr); err != nil {
			return fmt.Errorf("compression profile %s: %w", pr.Name, err)
		}
	}
	accounts, err := s.db.ListAccounts(ctx)
	if err != nil {
		return err
	}
	idOf := map[string]int64{}
	for _, a := range accounts {
		idOf[a.Namespace] = a.ID
	}
	profiles, err := s.db.ListCompressionProfiles(ctx)
	if err != nil {
		return err
	}
	profID := map[string]int64{}
	for _, pr := range profiles {
		profID[pr.Name] = pr.ID
	}
	for _, pc := range p.combos {
		c := pc.combo
		for i := range c.Hops {
			c.Hops[i].AccountID = idOf[pc.hopNS[i]]
		}
		c.AutoFreeAccounts = nil
		for _, ns := range pc.freeNS {
			c.AutoFreeAccounts = append(c.AutoFreeAccounts, idOf[ns])
		}
		for i := range c.AutoFreeInclude {
			c.AutoFreeInclude[i].AccountID = idOf[pc.inclNS[i]]
		}
		c.CompressionProfileID = nil
		if id, ok := profID[pc.profile]; ok && pc.profile != "" {
			c.CompressionProfileID = &id
		}
		if pc.existing != 0 {
			err = s.db.UpdateCombo(ctx, pc.existing, c)
		} else {
			_, err = s.db.UpsertCombo(ctx, c)
		}
		if err != nil {
			return fmt.Errorf("combo %s: %w", c.Name, err)
		}
	}
	if len(p.rules) > 0 {
		rules := make([]store.ModelRule, len(p.rules))
		for i, pr := range p.rules {
			rules[i] = pr.rule
			rules[i].ID, rules[i].AccountID = 0, idOf[pr.ns]
		}
		if _, _, err := s.db.ApplyModelRules(ctx, rules, nil); err != nil {
			return fmt.Errorf("model rules: %w", err)
		}
	}
	return nil
}

func hasEnabledKey(a store.AccountSummary) bool {
	for _, k := range a.Keys {
		if k.Enabled {
			return true
		}
	}
	return false
}

func accountInputFrom(a store.AccountSummary) store.AccountInput {
	enabled, session := a.Enabled, a.RequiresSession
	capTokens, rate := a.CapTokens, a.RateLimit
	return store.AccountInput{
		Name: a.Name, Namespace: a.Namespace, Kind: a.Kind, BaseURL: a.BaseURL,
		Enabled: &enabled, RequiresSession: &session, QuotaMode: a.QuotaMode,
		ProbeDelayMS: a.ProbeDelayMS, CapWindow: a.CapWindow, CapTokens: &capTokens,
		RateLimit: &rate, RateWindow: a.RateWindow, Notes: a.Notes,
		UsageConvention: string(a.UsageConvention),
	}
}

func accountChanges(cur, next store.AccountSummary) []string {
	var out []string
	diff := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, name)
		}
	}
	diff("name", cur.Name, next.Name)
	diff("kind", cur.Kind, next.Kind)
	diff("base_url", cur.BaseURL, next.BaseURL)
	diff("enabled", cur.Enabled, next.Enabled)
	diff("requires_session_header", cur.RequiresSession, next.RequiresSession)
	diff("quota_mode", cur.QuotaMode, next.QuotaMode)
	diff("cap_window", cur.CapWindow, next.CapWindow)
	diff("cap_tokens", cur.CapTokens, next.CapTokens)
	diff("rate_limit", cur.RateLimit, next.RateLimit)
	if next.RateLimit > 0 {
		diff("rate_window", cur.RateWindow, next.RateWindow)
	}
	if next.ProbeDelayMS != 0 {
		diff("probe_delay_ms", cur.ProbeDelayMS, next.ProbeDelayMS)
	}
	if next.Notes != "" {
		diff("notes", cur.Notes, next.Notes)
	}
	if next.UsageConvention != "" {
		diff("usage_convention", cur.UsageConvention, next.UsageConvention)
	}
	return out
}

func profileChanges(cur, next store.CompressionProfile) []string {
	var out []string
	if cur.Enabled != next.Enabled {
		out = append(out, "enabled")
	}
	if !jsonEqual(next.Stages, cur.Stages) {
		out = append(out, "stages")
	}
	if cur.ExemptLastTurn != next.ExemptLastTurn || cur.MinCompressRatio != next.MinCompressRatio ||
		cur.FailOpen != next.FailOpen || cur.AutoTriggerTokens != next.AutoTriggerTokens ||
		(next.FreshToolOutput != "" && cur.FreshToolOutput != next.FreshToolOutput) {
		out = append(out, "options")
	}
	if cur.Notes != next.Notes {
		out = append(out, "notes")
	}
	return out
}

func comboChanges(cur store.Combo, nsByID map[int64]string, curProfile string, next plannedCombo) []string {
	var out []string
	c := next.combo
	if cur.Strategy != c.Strategy || cur.StickyIdleS != c.StickyIdleS || cur.BlockSize != max(c.BlockSize, 1) {
		out = append(out, "strategy")
	}
	if cur.Enabled != c.Enabled {
		out = append(out, "enabled")
	}
	if curProfile != next.profile {
		out = append(out, "compression_profile")
	}
	if c.ContextSize != 0 && cur.ContextSize != c.ContextSize {
		out = append(out, "context_size")
	}
	if cur.Notes != c.Notes {
		out = append(out, "notes")
	}
	if cur.AutoFree != c.AutoFree {
		out = append(out, "auto_free")
	}
	type hop struct {
		at       string
		on, hold bool
	}
	view := func(hops []store.ComboHop, ns func(int) string) []hop {
		out := make([]hop, len(hops))
		for i, h := range hops {
			out[i] = hop{ns(i) + "/" + h.ModelID, h.Enabled == nil || *h.Enabled, h.HoldUntilFailure != nil && *h.HoldUntilFailure}
		}
		return out
	}
	if !c.AutoFree && !reflect.DeepEqual(
		view(cur.Hops, func(i int) string { return nsByID[cur.Hops[i].AccountID] }),
		view(c.Hops, func(i int) string { return next.hopNS[i] })) {
		out = append(out, "hops")
	}
	return out
}

func ruleChanged(cur, next store.ModelRule) bool {
	return cur.CapTokens != next.CapTokens || cur.CapWindow != next.CapWindow ||
		cur.WinStart != next.WinStart || cur.WinEnd != next.WinEnd || cur.WinDays != next.WinDays ||
		cur.WinTZ != next.WinTZ || cur.Enabled != next.Enabled || cur.Note != next.Note
}

// jsonEqual compares a raw JSON value with a decoded one (or two raw ones)
// by meaning, not by spacing or key order.
func jsonEqual(raw json.RawMessage, v any) bool {
	var a, b any
	if json.Unmarshal(raw, &a) != nil {
		return false
	}
	other, err := json.Marshal(v)
	if r, ok := v.(json.RawMessage); ok {
		other, err = r, nil
	}
	if err != nil || json.Unmarshal(bytes.TrimSpace(other), &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func orUnknown(ns string) string {
	if ns == "" {
		return "unknown account"
	}
	return ns
}
