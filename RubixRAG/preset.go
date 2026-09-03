package main

import "strings"

// ─────────────────────────────────────────────────────────────────────────────
// Presets (appSettings.Presets, settings.go) — a second, orthogonal
// restriction axis on top of SourceAccess: SourceAccess is about WHO
// (department), presets are about WHAT FOR (use case — a Chat question,
// the Agent tab, a Mail draft). Both apply together as an intersection; a
// preset can only narrow what SourceAccess already allows, never widen it.
// Same "absent/empty = unrestricted" convention as sourceAccessAllowed
// (settings.go), deliberately kept as a separate check rather than merged
// into the same map — the two concepts are independent and mixing them
// into one data structure would only obscure that.
// ─────────────────────────────────────────────────────────────────────────────

// findPreset looks up a preset by name (case-insensitive, matching
// sourceAccessAllowed's department-code comparison style). An empty name
// or no match returns the zero-value sourcePreset (empty Kinds/Tools —
// i.e. no restriction) rather than an error: an unset or stale
// DefaultPreset/DraftPreset/askRequest.Preset should degrade to "no
// preset restriction", not break the request.
func findPreset(presets []sourcePreset, name string) (sourcePreset, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return sourcePreset{}, false
	}
	for _, p := range presets {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return sourcePreset{}, false
}

// presetAllowsKind reports whether source_kind is within a preset's scope
// — an empty kinds list (no preset resolved, or a preset with Kinds left
// blank) means "no restriction on this axis", matching sourceAccessAllowed's
// "absent/empty = allowed" shape.
func presetAllowsKind(kinds []string, kind string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if strings.EqualFold(k, kind) {
			return true
		}
	}
	return false
}

// presetAllowsTool reports whether a tool category ("mssql", "shop",
// "http" — the last one gating every enabled entry in
// appSettings.HTTPTemplates as a group, not per-template) is within a
// preset's scope — same "empty = unrestricted" shape as presetAllowsKind.
func presetAllowsTool(tools []string, tool string) bool {
	if len(tools) == 0 {
		return true
	}
	for _, t := range tools {
		if strings.EqualFold(t, tool) {
			return true
		}
	}
	return false
}

// filterByPresetKinds drops candidate hits whose source_kind isn't within
// presetKinds — the preset-axis counterpart to rank.go's
// filterByDeptAccess, meant to run right alongside it (same "before
// scoring, so a denied chunk is never even a candidate" placement in
// rankedSearch).
func filterByPresetKinds(hits []rankedHit, presetKinds []string) []rankedHit {
	if len(presetKinds) == 0 {
		return hits
	}
	out := make([]rankedHit, 0, len(hits))
	for _, h := range hits {
		if presetAllowsKind(presetKinds, h.SourceKind) {
			out = append(out, h)
		}
	}
	return out
}
