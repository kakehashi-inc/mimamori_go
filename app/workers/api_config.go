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
	cfg, stored, err := modules.CentralConfigState(c.pool)
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

// applyConfigChanges checks and saves the changes in one transaction of the
// central database. It returns the names of the changed items.
func (c *core) applyConfigChanges(set map[string]string, unset []string) ([]string, error) {
	tx, err := c.pool.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := modules.ApplyCentralConfigChanges(tx, set, unset)
	if err != nil {
		return nil, err
	}
	return changed, tx.Commit()
}

// handleUpdateConfig sets and unsets items together. Items that apply while
// running take effect at once; the others when the central restarts.
func (c *core) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var in configUpdateJSON
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	changed, err := c.applyConfigChanges(in.Set, in.Unset)
	switch e := err.(type) {
	case nil:
	case modules.Problems:
		writeProblems(w, http.StatusBadRequest, modules.AsMsg(e), e)
		return
	case modules.Msg:
		writeError(w, http.StatusBadRequest, e)
		return
	default:
		// Anything else is a failure of the database, not a refused value.
		c.internalError(w, r, err)
		return
	}
	if len(changed) > 0 {
		// The history shows the values as saved (lists normalized).
		saved, _, savedErr := modules.CentralConfigState(c.pool)
		sort.Strings(changed)
		unset := map[string]bool{}
		for _, n := range in.Unset {
			unset[n] = true
		}
		var parts []modules.Msg
		for _, n := range changed {
			f, _ := modules.FindConfigField(n)
			switch {
			case unset[n]:
				parts = append(parts, modules.M("history.detail.config_default", "name", n))
			case f.Secret():
				parts = append(parts, modules.M("history.detail.config_secret", "name", n))
			case savedErr != nil:
				parts = append(parts, modules.M("history.detail.config_value", "name", n, "value", in.Set[n]))
			default:
				parts = append(parts, modules.M("history.detail.config_value", "name", n, "value", f.Value(saved)))
			}
		}
		c.history(principalFrom(r), models.HistConfig, "updated", strings.Join(changed, ", "), historyParts(parts))
		c.reloadConfig()
	}
	out, err := c.configOut()
	if err != nil {
		c.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
