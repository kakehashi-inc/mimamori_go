package workers

import (
	"net/http"
	"sort"
	"strings"

	"mimamori/app/models"
	"mimamori/app/modules"
)

type configItemJSON struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Value   string `json:"value"`   // empty for the secret item
	IsSet   bool   `json:"is_set"`  // the secret item has a value
	Default string `json:"default"` // empty for the secret item
	Stored  bool   `json:"stored"`  // false: the default applies
	Restart bool   `json:"restart"` // takes effect when the central restarts
	// Pending is true when a restart item's stored value differs from the
	// value the running central uses.
	Pending bool `json:"pending"`
}

type configJSON struct {
	Items []configItemJSON `json:"items"`
}

type configUpdateJSON struct {
	Set   map[string]string `json:"set"`
	Unset []string          `json:"unset"`
}

func (c *core) configOut() (*configJSON, error) {
	cfg, stored, err := modules.CentralConfigState(c.db)
	if err != nil {
		return nil, err
	}
	def := modules.DefaultCentralConfig()
	running := c.conf()
	out := &configJSON{Items: []configItemJSON{}}
	for i := range modules.ConfigFields {
		f := &modules.ConfigFields[i]
		item := configItemJSON{Name: f.Name, Kind: f.Kind, Stored: stored[f.Name], Restart: f.Restart}
		if f.Secret() {
			item.IsSet = f.Value(cfg) != ""
		} else {
			item.Value, item.Default = f.Value(cfg), f.Value(def)
		}
		if f.Restart && f.Value(cfg) != f.Value(running) {
			item.Pending = true
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func (c *core) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	out, err := c.configOut()
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUpdateConfig sets and unsets items together. Items that apply while
// running take effect at once; the others when the central restarts.
func (c *core) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var in configUpdateJSON
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	changed, err := modules.ApplyCentralConfigChanges(c.db, in.Set, in.Unset)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		unset := map[string]bool{}
		for _, n := range in.Unset {
			unset[n] = true
		}
		var parts []string
		for _, n := range changed {
			f, _ := modules.FindConfigField(n)
			switch {
			case unset[n]:
				parts = append(parts, n+" = (default)")
			case f.Secret():
				parts = append(parts, n+" changed")
			default:
				parts = append(parts, n+" = "+in.Set[n])
			}
		}
		c.history(principalFrom(r), models.HistConfig, "updated", strings.Join(changed, ", "), strings.Join(parts, "; "))
		c.reloadConfig()
	}
	out, err := c.configOut()
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
