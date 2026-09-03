package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Department classification, ported from external/zndz/lib/department.go
// (there, Auth() calls this with the raw AD "department" attribute — see
// zndz.go:431). Rubix's AD "department"/"title" attributes are free text
// entered over years by many different people (job titles, branch names,
// abbreviations), not a fixed vocabulary — this regex table normalizes
// that mess onto a small set of canonical department codes so
// settings.SourceAccess (see settings.go) has something stable to match
// against instead of every AD variant spelling.
//
// The ruleset is administrable: settings.PromptsDir/department_rules.json
// (same directory as index.md/skill_*.md — see skills.go), if present and
// valid, replaces defaultDepartmentRules below entirely. Re-read fresh on
// every login (see ldapAuthenticate), matching skills.go's "no caching,
// an edit takes effect immediately" reasoning — logins are infrequent
// enough that re-reading a small JSON file costs nothing. Missing file =
// fall back to the built-in defaults, exactly as the original zndz
// classifier codified; a file that exists but fails to parse or contains
// an invalid regex also falls back (logged as a warning), so a bad edit
// degrades to "the built-in rules still work" rather than breaking every
// login's classification outright.
// ─────────────────────────────────────────────────────────────────────────────

// departmentRule is one classification rule — also the on-disk JSON shape
// of department_rules.json (a plain array of these), so the admin-edited
// file and the built-in defaults share one representation.
type departmentRule struct {
	Regex string `json:"regex"`
	Code  string `json:"code"`
}

// departmentRulesFilename is the override file's name within
// promptsDirOrDefault(settings.PromptsDir) — see loadDepartmentRulesFromFile.
const departmentRulesFilename = "department_rules.json"

// defaultDepartmentRules is used whenever no valid department_rules.json
// override exists — ported verbatim from external/zndz/lib/department.go,
// including entry order: the loop returns the FIRST matching rule, so a
// later rule targeting the same or a different code can be unreachable if
// an earlier regex already matches the same text (e.g. the dedicated
// "Controlling" rule below is shadowed by the IT rule's "Controlling"
// alternative, which comes first) — that quirk is inherited from the
// original working classifier, not introduced here.
var defaultDepartmentRules = []departmentRule{
	{Regex: "(Gesch.+ftsf.+hr|CEO)", Code: "GF"},
	{Regex: "(Zoll|Export)", Code: "Zoll"},
	{Regex: "(Finanz|FiBu|Rechnungspr|reditoren|Fuhrpark|Complet|Bilanz|Accounting)", Code: "FiBu"},
	{Regex: "(Controlling|Fachinformatik|[- /]IT[- /]|EDV|Admin|Transformation Manager|Infrastructure)", Code: "IT"},
	{Regex: "(Qualit|[- /]QM[- /]|[- /]QMB?[- /])", Code: "QM"},
	{Regex: "(SAP|ERP|EWM)", Code: "SAP"},
	{Regex: "(Team Prozesse|Process)", Code: "Team Prozesse"},
	{Regex: "(E-Business)", Code: "EBusiness"},
	{Regex: "(Einkauf|Category Manager|Eink.+ufer|Datenmanagement)", Code: "Einkauf"},
	{Regex: "(Produktdaten|Teilemanage|Produktd|PDM)", Code: "PDM"},
	{Regex: "(Marketing)", Code: "Marketing"},
	{Regex: "(Personal)", Code: "Personal"},
	{Regex: "(Controlling)", Code: "Controlling"},
	{Regex: "(Empfang|Sekretariat)", Code: "Empfang"},
	{Regex: "(Industriekauf|Leitung Kundencenter|Handel|rokauf|Verkauf|Vertrieb|Sales|Kundenbetreu|Innendienst|Au.+endienst|Kaufm.+nnische|Reklamation|Leitung.Standort|Kundenberater|Standortleiter|Prokurist|utomotive|Key.*Account|[- /]KAM[- /]|Commercial|Augsburg|Berlin|Dortmund|Hannover|Kaiserslautern|Karlsruhe|Leipzig|Münster|Nürnberg|Stuttgart|Villingen|Wuppertal|Continuous Improvement|Improvement Manager|Business Development|Development Manager|Insite|Bereichsleiter)", Code: "Vertrieb"},
	{Regex: "(Transport|Lager|Logistik|AKL|Claim|Logistic Incoming Goods|Inbound|Shift|Dispatch)", Code: "Logistik"},
	{Regex: "(Leitung Key Account Management|Teamleitung|Technik/Service|Leitung Produktionen|Technischer Service|Fachberater|Projektmanagement|IPH Group|Junior Key Account Manager)", Code: "Spezial"},
	{Regex: "(Fertigung|Metall|Mechanik|Werkstatt|Montage|Produktionen)", Code: "Fertigung"},
	{Regex: "(Fahrdienst|Qualit.tssicherung|WA.Kontrolle)", Code: "Fahrdienst"},
	{Regex: "(Materialversorgung|Material Supply|Clearing|Shift|Warehousing|Lager|Transport|Logistik|Linear|Apprenticeship|Hydraulik Center|1. Stock|2. Stock|Versand|Wareneingang|Warenausgang|Hydraulikcenter|Added[- ]Value|konfektionierung|Workshop Plattling|Materialbereitstellung|Service Technican Specialist|Schichtleitung|Lagerleitstand|Logistic|Logistics)", Code: "Logistik"},
}

// defaultDepartmentCode is returned when nothing in the active ruleset
// matches — a logged-in user whose AD department/title just doesn't fit
// any rule, distinct from an anonymous caller (which callers represent
// as "", never this value — see classifyDepartment's doc comment).
const defaultDepartmentCode = "default"

// knownDepartmentCodes lists every canonical code defaultDepartmentRules
// can produce, derived rather than hand-maintained separately so it can't
// drift from the table above. Reflects only the built-in defaults (an
// admin-supplied department_rules.json can introduce codes not in this
// list) — used for tests and as a reference; the admin UI's hint text
// lists them statically, matching how the source_kind hint elsewhere in
// the settings UI is also static.
var knownDepartmentCodes = func() []string {
	codes := []string{defaultDepartmentCode}
	seen := map[string]bool{defaultDepartmentCode: true}
	for _, r := range defaultDepartmentRules {
		if !seen[r.Code] {
			seen[r.Code] = true
			codes = append(codes, r.Code)
		}
	}
	return codes
}()

// validateDepartmentRules rejects a ruleset containing an empty code or a
// regex that doesn't compile — called both when loading an on-disk
// override (loadDepartmentRulesFromFile) and when the admin UI saves one
// (see department_admin.go), so a typo fails loudly at save time rather
// than silently breaking every subsequent login's classification.
func validateDepartmentRules(rules []departmentRule) error {
	for i, r := range rules {
		if strings.TrimSpace(r.Code) == "" {
			return fmt.Errorf("rule %d: empty code", i)
		}
		if _, err := regexp.Compile(strings.ToUpper(r.Regex)); err != nil {
			return fmt.Errorf("rule %d (code %q): invalid regex %q: %w", i, r.Code, r.Regex, err)
		}
	}
	return nil
}

// loadDepartmentRulesFromFile reads and validates path as a
// department_rules.json override. ok=false (not an error) if the file
// doesn't exist — a fresh checkout or a deployment that never customized
// this falls back to defaultDepartmentRules silently, same as skills.go's
// loadManifest treating a missing manifest.json as "no skills yet", not a
// failure. A file that exists but fails to parse or validate is logged as
// a warning (an admin mistake worth surfacing) and also returns ok=false.
func loadDepartmentRulesFromFile(path string) (rules []departmentRule, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	if err := json.Unmarshal(b, &rules); err != nil {
		log.Printf("WARN: %s: invalid JSON (%v) — falling back to built-in department rules", path, err)
		return nil, false
	}
	if err := validateDepartmentRules(rules); err != nil {
		log.Printf("WARN: %s: %v — falling back to built-in department rules", path, err)
		return nil, false
	}
	return rules, true
}

// departmentRulesOrDefault resolves the effective ruleset for promptsDir
// (settings.PromptsDir) — a valid department_rules.json override if one
// exists there, else defaultDepartmentRules.
func departmentRulesOrDefault(promptsDir string) []departmentRule {
	path := filepath.Join(promptsDirOrDefault(promptsDir), departmentRulesFilename)
	if rules, ok := loadDepartmentRulesFromFile(path); ok {
		return rules
	}
	return defaultDepartmentRules
}

// classifyDepartment maps AD's free-text department/title attributes onto
// one of rules' canonical codes. department is tried first — the same
// field the original zndz code classifies on — falling back to title only
// if department doesn't match anything, since R3's LDAP login has both
// available (see ldapauth.go). Returns defaultDepartmentCode if neither
// matches any rule.
func classifyDepartment(rules []departmentRule, department, title string) string {
	if code := matchDepartmentRule(rules, department); code != defaultDepartmentCode {
		return code
	}
	if strings.TrimSpace(title) != "" {
		return matchDepartmentRule(rules, title)
	}
	return defaultDepartmentCode
}

// matchDepartmentRule runs rules against str, same matching shape as the
// original (case-insensitive, str padded with spaces so a regex anchored
// on word boundaries via "[- /]" still matches at the start/end of the
// string).
func matchDepartmentRule(rules []departmentRule, str string) string {
	upper := strings.ToUpper(" " + str + " ")
	for _, r := range rules {
		if m, _ := regexp.MatchString(strings.ToUpper(r.Regex), upper); m {
			return r.Code
		}
	}
	return defaultDepartmentCode
}
