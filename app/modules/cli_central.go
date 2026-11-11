package modules

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mimamori/app/models"
)

// CentralCmd groups the central's commands.
type CentralCmd struct {
	Start         CentralStartCmd         `cmd:"" help:"Start the central (browser screens, agent API, management API and the built-in agent)"`
	Stop          CentralStopCmd          `cmd:"" help:"Stop the running central (sends SIGTERM and waits until it has stopped)"`
	Status        CentralStatusCmd        `cmd:"" help:"Show whether the central is running (exit code 3 when it is not)"`
	InitAdmin     CentralInitAdminCmd     `cmd:"" name:"init-admin" help:"Create the first administrator (only while no administrator exists; run on the central host with access to its data directory)"`
	ResetPassword CentralResetPasswordCmd `cmd:"" name:"reset-password" help:"Reset an administrator's password (run on the central host with access to its data directory)"`
	Config        CentralConfigCmd        `cmd:"" help:"Show and change the configuration stored in the central database (run on the central host; also changeable on the settings screen)"`
}

// CentralStartCmd starts the central.
type CentralStartCmd struct{}

func (c *CentralStartCmd) Run() error {
	if StartCentral == nil {
		return NewExitError(ExitGeneral, "central not linked")
	}
	return StartCentral()
}

// localOperator names the OS user running a local administration command
// for the history.
func localOperator() string {
	if name, ok := ReadPasswdName(os.Getuid()); ok {
		return "local:" + name
	}
	return fmt.Sprintf("local:uid %d", os.Getuid())
}

// openCentralDBLocal opens the central database directly on the central
// host. Access is controlled by the permissions of the data directory.
func openCentralDBLocal() (*sql.DB, error) {
	dir := DataDir()
	if err := EnsureSecureDir(dir); err != nil {
		return nil, NewExitErrorf(ExitFileIO, "data directory %s: %v", dir, err)
	}
	db, err := OpenSQLite(filepath.Join(dir, CentralDBFileName), SchemaCentral)
	if err != nil {
		return nil, NewExitErrorf(ExitConfig, "database: %v", err)
	}
	return db, nil
}

func readNewPassword() (string, error) {
	p, err := ReadSecret("New password: ")
	if err != nil {
		return "", NewExitError(ExitArgument, err.Error())
	}
	if isTerminal(os.Stdin.Fd()) {
		again, err := ReadSecret("Repeat the password: ")
		if err != nil {
			return "", NewExitError(ExitArgument, err.Error())
		}
		if again != p {
			return "", NewExitError(ExitArgument, "the passwords do not match")
		}
	}
	if err := ValidatePassword(p); err != nil {
		return "", NewExitError(ExitArgument, err.Error())
	}
	return p, nil
}

// CentralInitAdminCmd creates the first administrator.
type CentralInitAdminCmd struct {
	Name     string `help:"Account name" required:""`
	Language string `help:"Display language of the browser screens: auto (follow the browser), en or ja" enum:"auto,en,ja" default:"auto"`
}

func (c *CentralInitAdminCmd) Run() error {
	db, err := openCentralDBLocal()
	if err != nil {
		return err
	}
	defer db.Close()
	n, err := models.CountAdmins(db)
	if err != nil {
		return err
	}
	if n > 0 {
		return NewExitError(ExitArgument, "an administrator already exists; use reset-password or the accounts screen")
	}
	p, err := readNewPassword()
	if err != nil {
		return err
	}
	hash, err := HashPassword(p)
	if err != nil {
		return err
	}
	a := &models.Account{Name: strings.TrimSpace(c.Name), Role: models.RoleAdmin, Language: c.Language, PasswordHash: hash}
	if err := models.InsertAccount(db, a); err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	_ = models.InsertHistory(db, &models.HistoryItem{AccountName: localOperator(), Category: models.HistAccount, Action: "created", Target: a.Name, Detail: "first administrator created on the central host"})
	fmt.Printf("Administrator %q created.\n", a.Name)
	return nil
}

// CentralResetPasswordCmd resets an administrator's password.
type CentralResetPasswordCmd struct {
	Name string `help:"Administrator account name" required:""`
}

func (c *CentralResetPasswordCmd) Run() error {
	db, err := openCentralDBLocal()
	if err != nil {
		return err
	}
	defer db.Close()
	a, err := models.GetAccountByName(db, c.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return NewExitErrorf(ExitArgument, "account %q not found", c.Name)
	}
	if err != nil {
		return err
	}
	if a.Role != models.RoleAdmin {
		return NewExitError(ExitArgument, "only administrators can be recovered here; administrators reset other accounts on the accounts screen")
	}
	p, err := readNewPassword()
	if err != nil {
		return err
	}
	hash, err := HashPassword(p)
	if err != nil {
		return err
	}
	if err := models.UpdateAccountPassword(db, a.ID, hash); err != nil {
		return err
	}
	_ = models.SetAccountDisabled(db, a.ID, false)
	_ = models.DeleteAccountSessions(db, a.ID, "")
	_ = models.InsertHistory(db, &models.HistoryItem{AccountName: localOperator(), Category: models.HistAccount, Action: "password_reset", Target: a.Name, Detail: "administrator recovered on the central host"})
	fmt.Printf("Password of %q reset; the account is enabled and its sessions were invalidated.\n", a.Name)
	return nil
}

// AgentCmd groups the agent's commands.
type AgentCmd struct {
	Init   AgentInitCmd   `cmd:"" help:"Initialize the agent (the token is read from standard input, never from an argument)"`
	Start  AgentStartCmd  `cmd:"" help:"Start the agent"`
	Stop   AgentStopCmd   `cmd:"" help:"Stop the running agent (sends SIGTERM and waits until it has finished its cycle)"`
	Status AgentStatusCmd `cmd:"" help:"Show whether the agent is running and its state (exit code 3 when it is not)"`
}

// AgentInitCmd initializes the agent.
type AgentInitCmd struct {
	Server        string `help:"Central URL, e.g. https://central.example.com:8443" required:""`
	Overwrite     bool   `help:"Replace an existing initialization"`
	CAFingerprint string `name:"ca-fingerprint" help:"Expected SHA-256 fingerprint of the central CA (shown on the setup guide screen)"`
}

func (c *AgentInitCmd) Run() error {
	return AgentInit(AgentInitOptions{
		ServerURL: c.Server, Overwrite: c.Overwrite, CAFingerprint: c.CAFingerprint, DataDir: DataDir(),
	})
}

// AgentStartCmd starts the agent.
type AgentStartCmd struct{}

func (c *AgentStartCmd) Run() error {
	return AgentStart(DataDir())
}
