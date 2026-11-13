package modules

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"mimamori/app/models"
)

// CentralConfigCmd groups the configuration commands. They work on the
// central database directly, so they also work while the central is stopped
// (for example to correct a listen address). A running central reloads the
// configuration periodically; items marked "restart" take effect when it
// restarts.
type CentralConfigCmd struct {
	List  CentralConfigListCmd  `cmd:"" help:"List every item with its value (items not set use their defaults)"`
	Get   CentralConfigGetCmd   `cmd:"" help:"Print the value of one item"`
	Set   CentralConfigSetCmd   `cmd:"" help:"Set one item (smtp.password is read from standard input)"`
	Unset CentralConfigUnsetCmd `cmd:"" help:"Remove one item so its default applies again"`
}

// effectNote tells when a change of the item takes effect.
func effectNote(f *ConfigField) string {
	if f.Restart {
		return "The change takes effect when the central is restarted."
	}
	return fmt.Sprintf("A running central applies the change at its next periodic check (every %s).", PeriodicTick)
}

// configChangeFailed is the error of a refused or failed change: the
// failure of its history row keeps its code; a refused value, as before,
// is an argument error.
func configChangeFailed(err error) error {
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return err
	}
	return NewExitError(ExitArgument, err.Error())
}

// CentralConfigListCmd lists the configuration.
type CentralConfigListCmd struct{}

func (c *CentralConfigListCmd) Run() error {
	db, err := openCentralDBLocal()
	if err != nil {
		return err
	}
	defer db.Close()
	cfg, stored, err := CentralConfigState(db)
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ITEM\tVALUE\tSOURCE\tAPPLIES\tDESCRIPTION")
	for i := range ConfigFields {
		f := &ConfigFields[i]
		v := f.Value(cfg)
		if f.Secret() {
			if v != "" {
				v = "(set)"
			} else {
				v = "(not set)"
			}
		}
		src := "default"
		if stored[f.Name] {
			src = "set"
		}
		applies := "running"
		if f.Restart {
			applies = "restart"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.Name, v, src, applies, f.Help)
	}
	return tw.Flush()
}

// CentralConfigGetCmd prints one item.
type CentralConfigGetCmd struct {
	Name string `arg:"" help:"Item name"`
}

func (c *CentralConfigGetCmd) Run() error {
	f, err := FindConfigField(c.Name)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	if f.Secret() {
		return NewExitErrorf(ExitArgument, "%s is not displayed", f.Name)
	}
	db, err := openCentralDBLocal()
	if err != nil {
		return err
	}
	defer db.Close()
	cfg, _, err := CentralConfigState(db)
	if err != nil {
		return NewExitError(ExitConfig, err.Error())
	}
	fmt.Println(f.Value(cfg))
	return nil
}

// CentralConfigSetCmd sets one item.
type CentralConfigSetCmd struct {
	Name  string `arg:"" help:"Item name"`
	Value string `arg:"" optional:"" help:"New value (lists are comma separated; omit for smtp.password)"`
}

func (c *CentralConfigSetCmd) Run() error {
	f, err := FindConfigField(c.Name)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	value := c.Value
	if f.Secret() {
		if value != "" {
			return NewExitErrorf(ExitArgument, "%s is read from standard input, not from an argument", f.Name)
		}
		if value, err = ReadSecret(f.Name + ": "); err != nil {
			return NewExitError(ExitArgument, err.Error())
		}
	} else if strings.TrimSpace(value) == "" && f.Kind != "string" && f.Kind != "list" {
		return NewExitErrorf(ExitArgument, "a value is required for %s", f.Name)
	}
	db, err := openCentralDBLocal()
	if err != nil {
		return err
	}
	defer db.Close()
	// The change and its history row are saved together or not at all.
	changed, err := ApplyCentralConfigChangesRecorded(db, map[string]string{f.Name: value}, nil, func(tx *sql.Tx, _ []string) error {
		detail := M("history.detail.config_secret", "name", f.Name)
		if !f.Secret() {
			// The history shows the value as saved (lists normalized).
			shown := value
			if saved, _, err := CentralConfigState(tx); err == nil {
				shown = f.Value(saved)
			}
			detail = M("history.detail.config_value", "name", f.Name, "value", shown)
		}
		return insertLocalHistory(tx, &models.HistoryItem{AccountName: localOperator(), Category: models.HistConfig, Action: "set", Target: f.Name,
			Detail: detail.Text(), I18n: HistoryTextsJSON(Msg{}, detail)})
	})
	if err != nil {
		return configChangeFailed(err)
	}
	if len(changed) == 0 {
		fmt.Printf("%s is unchanged.\n", f.Name)
		return nil
	}
	fmt.Printf("%s set. %s\n", f.Name, effectNote(f))
	return nil
}

// CentralConfigUnsetCmd removes one item.
type CentralConfigUnsetCmd struct {
	Name string `arg:"" help:"Item name"`
}

func (c *CentralConfigUnsetCmd) Run() error {
	db, err := openCentralDBLocal()
	if err != nil {
		return err
	}
	defer db.Close()
	f, err := FindConfigField(c.Name)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	// The change and its history row are saved together or not at all.
	changed, err := ApplyCentralConfigChangesRecorded(db, nil, []string{f.Name}, func(tx *sql.Tx, _ []string) error {
		restored := M("history.detail.config_restored")
		return insertLocalHistory(tx, &models.HistoryItem{AccountName: localOperator(), Category: models.HistConfig, Action: "unset", Target: c.Name,
			Detail: restored.Text(), I18n: HistoryTextsJSON(Msg{}, restored)})
	})
	if err != nil {
		return configChangeFailed(err)
	}
	if len(changed) == 0 {
		fmt.Printf("%s already uses its default.\n", f.Name)
		return nil
	}
	fmt.Printf("%s uses its default. %s\n", c.Name, effectNote(f))
	return nil
}
