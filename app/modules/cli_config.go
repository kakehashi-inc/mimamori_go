package modules

import (
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
	return "A running central applies the change within a minute."
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
	changed, err := ApplyCentralConfigChanges(db, map[string]string{f.Name: value}, nil)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	if len(changed) == 0 {
		fmt.Printf("%s is unchanged.\n", f.Name)
		return nil
	}
	detail := f.Name + " = " + value
	if f.Secret() {
		detail = f.Name + " changed"
	}
	_ = models.InsertHistory(db, &models.HistoryItem{AccountName: localOperator(), Category: models.HistConfig, Action: "set", Target: f.Name, Detail: detail})
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
	changed, err := ApplyCentralConfigChanges(db, nil, []string{f.Name})
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	if len(changed) == 0 {
		fmt.Printf("%s already uses its default.\n", f.Name)
		return nil
	}
	_ = models.InsertHistory(db, &models.HistoryItem{AccountName: localOperator(), Category: models.HistConfig, Action: "unset", Target: c.Name, Detail: "default restored"})
	fmt.Printf("%s uses its default. %s\n", c.Name, effectNote(f))
	return nil
}
