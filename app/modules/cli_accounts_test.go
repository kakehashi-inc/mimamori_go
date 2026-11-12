package modules

import (
	"testing"

	"github.com/alecthomas/kong"
)

// The account commands take the e-mail address and the display name.
func TestAccountCommandArguments(t *testing.T) {
	var cli struct {
		Ctl     CtlCmd     `cmd:""`
		Central CentralCmd `cmd:""`
	}
	parser, err := kong.New(&cli)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse([]string{"ctl", "accounts", "create", "a@example.com", "--display-name", "Alice", "--role", "viewer"}); err != nil {
		t.Fatal(err)
	}
	if c := cli.Ctl.Accounts.Create; c.Email != "a@example.com" || c.DisplayName != "Alice" || c.Role != "viewer" || c.Language != "auto" {
		t.Errorf("create: %+v", c)
	}
	if _, err := parser.Parse([]string{"ctl", "accounts", "email", "3", "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	if c := cli.Ctl.Accounts.Email; c.ID != 3 || c.Email != "b@example.com" {
		t.Errorf("email: %+v", c)
	}
	if _, err := parser.Parse([]string{"ctl", "accounts", "display-name", "3", "Bob B"}); err != nil {
		t.Fatal(err)
	}
	if c := cli.Ctl.Accounts.DisplayName; c.ID != 3 || c.Name != "Bob B" {
		t.Errorf("display-name: %+v", c)
	}
	if _, err := parser.Parse([]string{"central", "init-admin", "--email", "root@localhost", "--display-name", "Root"}); err != nil {
		t.Fatal(err)
	}
	if c := cli.Central.InitAdmin; c.Email != "root@localhost" || c.DisplayName != "Root" || c.Language != "auto" {
		t.Errorf("init-admin: %+v", c)
	}
	if _, err := parser.Parse([]string{"central", "reset-password", "--email", "root@localhost"}); err != nil {
		t.Fatal(err)
	}
	if cli.Central.ResetPassword.Email != "root@localhost" {
		t.Errorf("reset-password: %+v", cli.Central.ResetPassword)
	}
	if _, err := parser.Parse([]string{"central", "reset-password", "--name", "root"}); err == nil {
		t.Error("reset-password --name must be gone")
	}
	// The display name is required (checked with a fresh parser each time).
	for _, args := range [][]string{
		{"ctl", "accounts", "create", "a@example.com", "--role", "viewer"},
		{"central", "init-admin", "--email", "root@localhost"},
	} {
		var fresh struct {
			Ctl     CtlCmd     `cmd:""`
			Central CentralCmd `cmd:""`
		}
		p, err := kong.New(&fresh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Parse(args); err == nil {
			t.Errorf("%v: --display-name must be required", args)
		}
	}
}
