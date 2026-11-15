package workers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"mimamori/app/models"
	"mimamori/app/modules"
)

// A filter of a list takes only the values the program defines: every one
// of them (and the filter left out or empty) answers the list; any other
// value is refused with 400 and a message that names the parameter and
// lists the accepted values, as the other query checks of the handler.
func TestAPIFilterValues(t *testing.T) {
	e := newTestEnv(t)
	for _, tc := range []struct {
		path     string // the list with "?name="
		name     string
		accepted []string
	}{
		{"/history?category=", "category", models.HistoryCategories},
		{"/detections?category=", "category", modules.DetectionCategories},
		{"/detections?type=", "type", modules.DetectionTypes},
	} {
		for _, v := range append([]string{""}, tc.accepted...) {
			if rec := e.cli("GET", tc.path+v, nil); rec.Code != http.StatusOK {
				t.Errorf("%s%s: %d %s", tc.path, v, rec.Code, rec.Body.String())
			}
		}
		for raw, value := range map[string]string{
			"bogus":                         "bogus",
			strings.ToUpper(tc.accepted[0]): strings.ToUpper(tc.accepted[0]),
			"%20" + tc.accepted[0]:          " " + tc.accepted[0],
			tc.accepted[0] + "%20":          tc.accepted[0] + " ",
			tc.accepted[0] + ",":            tc.accepted[0] + ",",
		} {
			rec := e.cli("GET", tc.path+raw, nil)
			var body struct {
				Error     string `json:"error"`
				ErrorI18n struct {
					Code string `json:"code"`
				} `json:"error_i18n"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			want := "invalid " + tc.name + " " + strconv.Quote(value) + ": use one of " + strings.Join(tc.accepted, ", ")
			if rec.Code != http.StatusBadRequest || body.ErrorI18n.Code != "common.query_not_one_of" || body.Error != want {
				t.Errorf("%s%s: %d %s, want 400 %q", tc.path, raw, rec.Code, rec.Body.String(), want)
			}
		}
	}
}
