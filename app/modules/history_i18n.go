package modules

import "encoding/json"

// HistoryTexts is the coded target and detail of a history row, kept as
// JSON in the column history.i18n next to their English renderings (the
// columns target and detail). A part that is a plain value (a name, an
// address) or empty is not kept: the English is shown as it is.
type HistoryTexts struct {
	Target *Msg `json:"target,omitempty"`
	Detail *Msg `json:"detail,omitempty"`
}

// HistoryTextsJSON returns the JSON stored in history.i18n for the target
// and detail of a row: only their coded parts (an empty string when neither
// has a code).
func HistoryTextsJSON(target, detail Msg) string {
	var t HistoryTexts
	if target.Code != "" {
		t.Target = &target
	}
	if detail.Code != "" {
		t.Detail = &detail
	}
	if t.Target == nil && t.Detail == nil {
		return ""
	}
	b, err := json.Marshal(t)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseHistoryTexts returns the coded target and detail stored for a row
// whose English target and detail are given. A coded part is returned only
// when its English rendering is the stored English, so a screen never shows
// a text that differs from the recorded one; a row without the column's
// value (or with a damaged one) gives no coded parts.
func ParseHistoryTexts(stored, target, detail string) HistoryTexts {
	var t HistoryTexts
	if stored == "" || json.Unmarshal([]byte(stored), &t) != nil {
		return HistoryTexts{}
	}
	if t.Target != nil && (t.Target.Code == "" || t.Target.Text() != target) {
		t.Target = nil
	}
	if t.Detail != nil && (t.Detail.Code == "" || t.Detail.Text() != detail) {
		t.Detail = nil
	}
	return t
}
