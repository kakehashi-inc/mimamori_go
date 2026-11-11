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
// (for example to correct a listen address).
type CentralConfigCmd struct {
	List  CentralConfigListCmd  `cmd:"" help:"List every item with its value (items not set use their defaults)"`
	Get   CentralConfigGetCmd   `cmd:"" help:"Print the value of one item"`
	Set   CentralConfigSetCmd   `cmd:"" help:"Set one item (smtp.password is read from standard input)"`
	Unset CentralConfigUnsetCmd `cmd:"" help:"Remove one item so its default applies again"`
}

const restartNote = "The change takes effect when the central is restarted."

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
	fmt.Fprintln(tw, "ITEM\tVALUE\tSOURCE\tDESCRIPTION")
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.Name, v, src, f.Help)
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
	if err := SetCentralConfigValue(db, f.Name, value); err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	detail := f.Name + " = " + value
	if f.Secret() {
		detail = f.Name + " changed"
	}
	_ = models.InsertHistory(db, &models.HistoryItem{AccountName: localOperator(), Category: models.HistConfig, Action: "set", Target: f.Name, Detail: detail})
	fmt.Printf("%s set. %s\n", f.Name, restartNote)
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
	if err := UnsetCentralConfigValue(db, c.Name); err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	_ = models.InsertHistory(db, &models.HistoryItem{AccountName: localOperator(), Category: models.HistConfig, Action: "unset", Target: c.Name, Detail: "default restored"})
	fmt.Printf("%s uses its default. %s\n", c.Name, restartNote)
	return nil
}
