package modules

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Categories of a detection.
const (
	CategoryAlert  = "alert"
	CategoryNormal = "normal"
)

// IfMissing markers of a rule element.
const (
	IfMissingIgnore = "ignore" // silently exclude when the item does not exist
	IfMissingWarn   = "warn"   // exclude and report a warning
)

// Detection types.
const (
	TypeExec     = "exec"
	TypeDenied   = "denied"
	TypeRead     = "read"
	TypeNetwork  = "network"
	TypeWritable = "writable"
	TypeExecFile = "exec_file"
)

// DetectionTypes lists every detection type.
var DetectionTypes = []string{TypeExec, TypeDenied, TypeRead, TypeNetwork, TypeWritable, TypeExecFile}

// Element is one OS user, directory or executable with its if-missing marker.
type Element struct {
	Value     string `json:"value"`
	IfMissing string `json:"if_missing"`
}

// CategoryOnly is a monitoring type whose only own item is the category.
type CategoryOnly struct {
	Category string `json:"category"`
}

// ReadSpec configures read monitoring.
type ReadSpec struct {
	Dirs     []Element `json:"dirs"`
	Category string    `json:"category"`
}

// NetworkSpec configures network monitoring.
type NetworkSpec struct {
	Executables []Element `json:"executables"`
	Category    string    `json:"category"`
}

// WritableSpec configures the writable-state scan. Base is the scan origin
// shared with the exec-file scan.
type WritableSpec struct {
	Base         string   `json:"base"`
	AllowedPaths []string `json:"allowed_paths"`
	Category     string   `json:"category"`
}

// LegitLocation is a place where files with the given extensions are created
// legitimately, and the category used for them.
type LegitLocation struct {
	Path       string   `json:"path"`
	Extensions []string `json:"extensions"`
	Category   string   `json:"category"`
}

// ExecFilesSpec configures the executed-file scan inside the allowed paths.
type ExecFilesSpec struct {
	Extensions    []string        `json:"extensions"`
	Legit         []LegitLocation `json:"legit"`
	OtherCategory string          `json:"other_category"`
}

// RuleContent is the value-only content of a monitoring rule (and of a
// template). It can never carry auditd rule text or global auditd settings:
// the agent generates those from fixed templates.
type RuleContent struct {
	Users              []Element     `json:"users"`
	ExcludeInteractive *bool         `json:"exclude_interactive"`
	CheckIntervalSec   int           `json:"check_interval_sec"`
	ScanIntervalSec    int           `json:"scan_interval_sec"`
	Exec               CategoryOnly  `json:"exec"`
	Denied             CategoryOnly  `json:"denied"`
	Read               ReadSpec      `json:"read"`
	Network            NetworkSpec   `json:"network"`
	Writable           WritableSpec  `json:"writable"`
	ExecFiles          ExecFilesSpec `json:"exec_files"`
}

// ParseRuleContent decodes JSON strictly (unknown fields are rejected) and
// normalizes the result.
func ParseRuleContent(data []byte) (*RuleContent, error) {
	if !IsJSONObject(data) {
		return nil, M("rule.content.not_object")
	}
	var c RuleContent
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			var field any = te.Field
			if te.Field == "" {
				field = M("json.kind.the_content")
			}
			return nil, M("rule.content.wrong_type", "field", field, "expected", jsonKindName(te.Type), "found", te.Value)
		}
		return nil, M("rule.content.decode", "detail", strings.TrimPrefix(err.Error(), "json: "))
	}
	c.Normalize()
	return &c, nil
}

// IsJSONObject reports whether data holds a JSON object (not null, an array
// or a scalar value).
func IsJSONObject(data []byte) bool {
	t := strings.TrimSpace(string(data))
	return strings.HasPrefix(t, "{")
}

// jsonKindName names the JSON form expected for a Go type, for messages
// that must not expose Go type names.
func jsonKindName(t reflect.Type) Msg {
	if t == nil {
		return M("json.kind.valid")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return M("json.kind.array")
	case reflect.Struct, reflect.Map:
		return M("json.kind.object")
	case reflect.String:
		return M("json.kind.string")
	case reflect.Bool:
		return M("json.kind.bool")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return M("json.kind.integer")
	case reflect.Float32, reflect.Float64:
		return M("json.kind.number")
	}
	return M("json.kind.valid")
}

// JSON returns the canonical indented JSON of the content.
func (c *RuleContent) JSON() string {
	b, _ := json.MarshalIndent(c, "", "  ")
	return string(b)
}

// Clone returns a deep copy.
func (c *RuleContent) Clone() *RuleContent {
	out, _ := ParseRuleContent([]byte(c.JSON()))
	if out == nil {
		return &RuleContent{}
	}
	return out
}

// Normalize trims values, removes duplicates and replaces nil slices with
// empty ones so the JSON form is stable.
func (c *RuleContent) Normalize() {
	c.Users = normElements(c.Users)
	c.Read.Dirs = normElements(c.Read.Dirs)
	c.Network.Executables = normElements(c.Network.Executables)
	c.Writable.Base = strings.TrimSpace(c.Writable.Base)
	c.Writable.AllowedPaths = normStrings(c.Writable.AllowedPaths, normRelPath)
	c.ExecFiles.Extensions = normStrings(c.ExecFiles.Extensions, normExt)
	legit := make([]LegitLocation, 0, len(c.ExecFiles.Legit))
	seen := map[string]bool{}
	for _, l := range c.ExecFiles.Legit {
		l.Path = normRelPath(l.Path)
		l.Extensions = normStrings(l.Extensions, normExt)
		l.Category = strings.TrimSpace(l.Category)
		k := legitKey(l)
		if l.Path == "" || seen[k] {
			continue
		}
		seen[k] = true
		legit = append(legit, l)
	}
	c.ExecFiles.Legit = legit
	for _, p := range []*string{&c.Exec.Category, &c.Denied.Category, &c.Read.Category, &c.Network.Category, &c.Writable.Category, &c.ExecFiles.OtherCategory} {
		*p = strings.TrimSpace(*p)
	}
}

func legitKey(l LegitLocation) string {
	return l.Path + "|" + strings.Join(l.Extensions, ",")
}

func normElements(in []Element) []Element {
	out := make([]Element, 0, len(in))
	idx := map[string]int{}
	for _, e := range in {
		e.Value = strings.TrimSpace(e.Value)
		if e.Value == "" {
			continue
		}
		if e.IfMissing != IfMissingWarn {
			e.IfMissing = IfMissingIgnore
		}
		if i, ok := idx[e.Value]; ok {
			// A "warn" marker wins over "ignore" when both are given.
			if e.IfMissing == IfMissingWarn {
				out[i].IfMissing = IfMissingWarn
			}
			continue
		}
		idx[e.Value] = len(out)
		out = append(out, e)
	}
	return out
}

func normStrings(in []string, f func(string) string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = f(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func normRelPath(s string) string {
	return strings.Trim(strings.TrimSpace(s), "/")
}

func normExt(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s != "" && !strings.HasPrefix(s, ".") {
		s = "." + s
	}
	return s
}

var (
	userNameRe  = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}\$?$`)
	extensionRe = regexp.MustCompile(`^\.[a-z0-9]{1,15}$`)
	componentRe = regexp.MustCompile(`^[^/\x00]+$`)
)

// ValidateAbsPath checks that p is a clean absolute path without "..".
func ValidateAbsPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return M("rule.content.path_not_absolute", "path", strconv.Quote(p))
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." {
			return M("rule.content.path_dot", "path", strconv.Quote(p))
		}
	}
	if path.Clean(p) != p {
		return M("rule.content.path_not_clean", "path", strconv.Quote(p))
	}
	// The path becomes one argument of an auditd rule line, which is split
	// at white space: no white space (in any script), control character or
	// quote is allowed.
	for _, r := range p {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '\'' {
			return M("rule.content.path_char", "path", strconv.Quote(p))
		}
	}
	if len(p) > 4096 {
		return M("rule.content.path_too_long", "path", strconv.Quote(p))
	}
	return nil
}

// ValidateRelPath checks a directory-name sequence such as "bootstrap/cache".
func ValidateRelPath(p string) error {
	if p == "" {
		return M("rule.content.relpath_empty")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || !componentRe.MatchString(part) {
			return M("rule.content.relpath_invalid", "path", strconv.Quote(p))
		}
	}
	return nil
}

func validCategory(c string, allowEmpty bool) bool {
	return c == CategoryAlert || c == CategoryNormal || (allowEmpty && c == "")
}

// Validate returns every problem found in the content (empty when valid),
// sorted by their English text.
func (c *RuleContent) Validate() []string {
	return ProblemTexts(c.Problems())
}

// Problems returns every problem found in the content as coded messages,
// sorted by their English text (the order of Validate).
func (c *RuleContent) Problems() []Problem {
	var errs []Problem
	add := func(field string, m Msg) { errs = append(errs, FieldProblem(field, m)) }
	entry := func(name, value string, m Msg) {
		errs = append(errs, Problem{Where: []Msg{M("loc.entry", "name", name, "value", strconv.Quote(value))}, Msg: m})
	}
	tooMany := func(field string, limit int) { add(field, M("rule.content.too_many", "max", limit)) }
	if len(c.Users) > MaxRuleElements {
		tooMany("users", MaxRuleElements)
	}
	for _, u := range c.Users {
		if !userNameRe.MatchString(u.Value) {
			add("users", M("rule.content.user_invalid", "value", strconv.Quote(u.Value)))
		}
	}
	if c.CheckIntervalSec != 0 && (c.CheckIntervalSec < MinCheckInterval || c.CheckIntervalSec > MaxCheckInterval) {
		add("check_interval_sec", M("rule.content.range", "min", MinCheckInterval, "max", MaxCheckInterval))
	}
	if c.ScanIntervalSec != 0 && (c.ScanIntervalSec < MinScanInterval || c.ScanIntervalSec > MaxScanInterval) {
		add("scan_interval_sec", M("rule.content.range", "min", MinScanInterval, "max", MaxScanInterval))
	}
	for name, cat := range map[string]string{
		"exec.category": c.Exec.Category, "denied.category": c.Denied.Category, "read.category": c.Read.Category,
		"network.category": c.Network.Category, "writable.category": c.Writable.Category,
		"exec_files.other_category": c.ExecFiles.OtherCategory,
	} {
		if !validCategory(cat, true) {
			add(name, M("rule.content.category_invalid"))
		}
	}
	if len(c.Read.Dirs) > MaxRuleElements {
		tooMany("read.dirs", MaxRuleElements)
	}
	for _, d := range c.Read.Dirs {
		if err := ValidateAbsPath(d.Value); err != nil {
			add("read.dirs", AsMsg(err))
		}
	}
	if len(c.Network.Executables) > MaxRuleElements {
		tooMany("network.executables", MaxRuleElements)
	}
	for _, e := range c.Network.Executables {
		if err := ValidateAbsPath(e.Value); err != nil {
			add("network.executables", AsMsg(err))
		}
	}
	if c.Writable.Base != "" {
		if err := ValidateAbsPath(c.Writable.Base); err != nil {
			add("writable.base", AsMsg(err))
		} else if c.Writable.Base == "/" {
			add("writable.base", M("rule.content.base_root"))
		}
	}
	if len(c.Writable.AllowedPaths) > MaxRuleElements {
		tooMany("writable.allowed_paths", MaxRuleElements)
	}
	for _, p := range c.Writable.AllowedPaths {
		if err := ValidateRelPath(p); err != nil {
			add("writable.allowed_paths", AsMsg(err))
		}
	}
	if len(c.ExecFiles.Extensions) > MaxExtensions {
		tooMany("exec_files.extensions", MaxExtensions)
	}
	for _, e := range c.ExecFiles.Extensions {
		if !extensionRe.MatchString(e) {
			add("exec_files.extensions", M("rule.content.extension_invalid", "value", strconv.Quote(e)))
		}
	}
	if len(c.ExecFiles.Legit) > MaxRuleElements {
		tooMany("exec_files.legit", MaxRuleElements)
	}
	for _, l := range c.ExecFiles.Legit {
		if err := ValidateRelPath(l.Path); err != nil {
			add("exec_files.legit", AsMsg(err))
		}
		if len(l.Extensions) == 0 {
			entry("exec_files.legit", l.Path, M("rule.content.legit_extension_required"))
		}
		for _, e := range l.Extensions {
			if !extensionRe.MatchString(e) {
				entry("exec_files.legit", l.Path, M("rule.content.extension_invalid", "value", strconv.Quote(e)))
			}
		}
		if !validCategory(l.Category, false) {
			entry("exec_files.legit", l.Path, M("rule.content.legit_category"))
		}
	}
	SortProblems(errs)
	return errs
}

// --- Enabled types ---

// ExecEnabled and the following report whether a monitoring type is active.
// A type whose items were not specified is not performed.
func (c *RuleContent) ExecEnabled() bool   { return len(c.Users) > 0 && c.Exec.Category != "" }
func (c *RuleContent) DeniedEnabled() bool { return len(c.Users) > 0 && c.Denied.Category != "" }
func (c *RuleContent) ReadEnabled() bool {
	return len(c.Users) > 0 && len(c.Read.Dirs) > 0 && c.Read.Category != ""
}
func (c *RuleContent) NetworkEnabled() bool {
	return len(c.Network.Executables) > 0 && c.Network.Category != ""
}
func (c *RuleContent) WritableEnabled() bool {
	return c.Writable.Base != "" && c.Writable.Category != ""
}
func (c *RuleContent) ExecFilesEnabled() bool {
	return c.Writable.Base != "" && len(c.Writable.AllowedPaths) > 0 &&
		len(c.ExecFiles.Extensions) > 0 && c.ExecFiles.OtherCategory != ""
}

// CheckInterval returns the effective check interval in seconds.
func (c *RuleContent) CheckInterval() int {
	if c.CheckIntervalSec > 0 {
		return c.CheckIntervalSec
	}
	return DefaultCheckIntervalSec
}

// ScanInterval returns the effective writable-scan interval in seconds.
func (c *RuleContent) ScanInterval() int {
	if c.ScanIntervalSec > 0 {
		return c.ScanIntervalSec
	}
	return DefaultScanIntervalSec
}

// IsExcludeInteractive returns the effective exclude-interactive flag (default on).
func (c *RuleContent) IsExcludeInteractive() bool {
	return c.ExcludeInteractive == nil || *c.ExcludeInteractive
}

// --- Template merge ---

// NamedContent is a template name with its content.
type NamedContent struct {
	Name    string
	Content *RuleContent
}

// MergeOption is one candidate value of a conflicting field.
type MergeOption struct {
	Template string `json:"template"`
	Value    any    `json:"value"`
}

// MergeConflict is a single-value field that differs between templates.
type MergeConflict struct {
	Field   string        `json:"field"`
	Options []MergeOption `json:"options"`
}

type scalarField struct {
	name string
	get  func(c *RuleContent) any // nil when not specified
	set  func(c *RuleContent, v any)
}

func strField(name string, p func(c *RuleContent) *string) scalarField {
	return scalarField{
		name: name,
		get: func(c *RuleContent) any {
			if v := *p(c); v != "" {
				return v
			}
			return nil
		},
		set: func(c *RuleContent, v any) { *p(c) = v.(string) },
	}
}

func intField(name string, p func(c *RuleContent) *int) scalarField {
	return scalarField{
		name: name,
		get: func(c *RuleContent) any {
			if v := *p(c); v != 0 {
				return v
			}
			return nil
		},
		set: func(c *RuleContent, v any) { *p(c) = v.(int) },
	}
}

var scalarFields = []scalarField{
	{
		name: "exclude_interactive",
		get: func(c *RuleContent) any {
			if c.ExcludeInteractive == nil {
				return nil
			}
			return *c.ExcludeInteractive
		},
		set: func(c *RuleContent, v any) { b := v.(bool); c.ExcludeInteractive = &b },
	},
	intField("check_interval_sec", func(c *RuleContent) *int { return &c.CheckIntervalSec }),
	intField("scan_interval_sec", func(c *RuleContent) *int { return &c.ScanIntervalSec }),
	strField("exec.category", func(c *RuleContent) *string { return &c.Exec.Category }),
	strField("denied.category", func(c *RuleContent) *string { return &c.Denied.Category }),
	strField("read.category", func(c *RuleContent) *string { return &c.Read.Category }),
	strField("network.category", func(c *RuleContent) *string { return &c.Network.Category }),
	strField("writable.base", func(c *RuleContent) *string { return &c.Writable.Base }),
	strField("writable.category", func(c *RuleContent) *string { return &c.Writable.Category }),
	strField("exec_files.other_category", func(c *RuleContent) *string { return &c.ExecFiles.OtherCategory }),
}

// MergeTemplates combines templates into one rule content. List items are
// united without duplicates. A single-value field set to different values by
// several templates is a conflict: choices maps the field to the template
// whose value is used. Unresolved conflicts take the first template's value
// and are returned so the caller can ask the administrator.
func MergeTemplates(items []NamedContent, choices map[string]string) (*RuleContent, []MergeConflict) {
	out := &RuleContent{}
	var conflicts []MergeConflict
	for _, it := range items {
		c := it.Content
		out.Users = append(out.Users, c.Users...)
		out.Read.Dirs = append(out.Read.Dirs, c.Read.Dirs...)
		out.Network.Executables = append(out.Network.Executables, c.Network.Executables...)
		out.Writable.AllowedPaths = append(out.Writable.AllowedPaths, c.Writable.AllowedPaths...)
		out.ExecFiles.Extensions = append(out.ExecFiles.Extensions, c.ExecFiles.Extensions...)
	}
	for _, f := range scalarFields {
		var opts []MergeOption
		distinct := map[string]bool{}
		for _, it := range items {
			v := f.get(it.Content)
			if v == nil {
				continue
			}
			opts = append(opts, MergeOption{Template: it.Name, Value: v})
			distinct[fmt.Sprint(v)] = true
		}
		if len(opts) == 0 {
			continue
		}
		chosen := opts[0].Value
		if len(distinct) > 1 {
			resolved := false
			if want, ok := choices[f.name]; ok {
				for _, o := range opts {
					if o.Template == want {
						chosen, resolved = o.Value, true
						break
					}
				}
			}
			if !resolved {
				conflicts = append(conflicts, MergeConflict{Field: f.name, Options: opts})
			}
		}
		f.set(out, chosen)
	}
	// Legit locations: united by path and extensions; a category conflict is
	// resolved like a single-value field.
	type legitAcc struct {
		loc  LegitLocation
		opts []MergeOption
		cats map[string]bool
	}
	var order []string
	acc := map[string]*legitAcc{}
	for _, it := range items {
		for _, l := range it.Content.ExecFiles.Legit {
			k := legitKey(l)
			a, ok := acc[k]
			if !ok {
				a = &legitAcc{loc: l, cats: map[string]bool{}}
				acc[k] = a
				order = append(order, k)
			}
			a.opts = append(a.opts, MergeOption{Template: it.Name, Value: l.Category})
			a.cats[l.Category] = true
		}
	}
	for _, k := range order {
		a := acc[k]
		field := "exec_files.legit:" + k
		if len(a.cats) > 1 {
			resolved := false
			if want, ok := choices[field]; ok {
				for _, o := range a.opts {
					if o.Template == want {
						a.loc.Category, resolved = o.Value.(string), true
						break
					}
				}
			}
			if !resolved {
				a.loc.Category = a.opts[0].Value.(string)
				conflicts = append(conflicts, MergeConflict{Field: field, Options: a.opts})
			}
		}
		out.ExecFiles.Legit = append(out.ExecFiles.Legit, a.loc)
	}
	out.Normalize()
	return out, conflicts
}

// --- Path matching ---

// SplitComponents splits a slash path into its non-empty components.
func SplitComponents(p string) []string {
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// MatchSeqAt reports the index just past the first occurrence of seq as
// consecutive components in comps, or -1. Matching ignores depth, so
// "storage" matches /opt/a/storage and /opt/x/a/prod/storage alike.
func MatchSeqAt(comps, seq []string) int {
	if len(seq) == 0 || len(seq) > len(comps) {
		return -1
	}
outer:
	for i := 0; i+len(seq) <= len(comps); i++ {
		for j := range seq {
			if comps[i+j] != seq[j] {
				continue outer
			}
		}
		return i + len(seq)
	}
	return -1
}

// EndsWithSeq reports whether comps ends with seq.
func EndsWithSeq(comps, seq []string) bool {
	if len(seq) == 0 || len(seq) > len(comps) {
		return false
	}
	off := len(comps) - len(seq)
	for j := range seq {
		if comps[off+j] != seq[j] {
			return false
		}
	}
	return true
}
