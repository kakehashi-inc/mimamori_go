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
	if err := EnsureDataDir(dir); err != nil {
		return nil, NewExitErrorf(ExitFileIO, "data directory %s: %v", dir, err)
	}
	db, err := OpenSQLite(filepath.Join(dir, CentralDBFileName), SchemaCentral)
	if err != nil {
		return nil, DBOpenFailed(err, ExitConfig, "database")
	}
	return db, nil
}

// withLocalTx runs fn in one transaction of the central database: its
// changes are saved only when fn succeeds. Errors are returned as they are
// (as before the transaction, a failing statement ends the command with its
// own text).
func withLocalTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// insertLocalHistory records the history row of a command on the central
// host; its failure undoes the change it records.
func insertLocalHistory(tx *sql.Tx, h *models.HistoryItem) error {
	if err := models.InsertHistory(tx, h); err != nil {
		return NewExitErrorf(ExitConfig, "history: %v (nothing was changed)", err)
	}
	return nil
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
	Email       string `help:"E-mail address (the login ID)" required:""`
	DisplayName string `name:"display-name" help:"Display name shown on the screens" required:""`
	Language    string `help:"Display language of the browser screens: auto (follow the browser), en or ja" enum:"auto,en,ja" default:"auto"`
}

func (c *CentralInitAdminCmd) Run() error {
	email, err := ValidateEmail(c.Email)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
	display, err := ValidateDisplayName(c.DisplayName)
	if err != nil {
		return NewExitError(ExitArgument, err.Error())
	}
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
	a := &models.Account{Email: email, DisplayName: display, Role: models.RoleAdmin, Language: c.Language, PasswordHash: hash}
	// The account and its history row are written together or not at all.
	err = withLocalTx(db, func(tx *sql.Tx) error {
		// Checked again under the write lock: another command may have
		// created an administrator while the password was typed.
		if n, err := models.CountAdmins(tx); err != nil {
			return err
		} else if n > 0 {
			return NewExitError(ExitArgument, "an administrator already exists; use reset-password or the accounts screen")
		}
		if err := models.InsertAccount(tx, a); err != nil {
			return NewExitError(ExitArgument, err.Error())
		}
		detail := M("history.detail.first_admin_cli")
		return insertLocalHistory(tx, &models.HistoryItem{AccountName: localOperator(), Category: models.HistAccount, Action: "created", Target: a.Email,
			Detail: detail.Text(), I18n: HistoryTextsJSON(Msg{}, detail)})
	})
	if err != nil {
		return err
	}
	fmt.Printf("Administrator %q created.\n", a.Email)
	return nil
}

// CentralResetPasswordCmd resets an administrator's password.
type CentralResetPasswordCmd struct {
	Email string `help:"E-mail address (login ID) of the administrator" required:""`
}

func (c *CentralResetPasswordCmd) Run() error {
	db, err := openCentralDBLocal()
	if err != nil {
		return err
	}
	defer db.Close()
	email := strings.TrimSpace(c.Email)
	a, err := models.GetAccountByEmail(db, email)
	if errors.Is(err, sql.ErrNoRows) {
		return NewExitErrorf(ExitArgument, "account %q not found", email)
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
	// The new password, the enabling, the revocation and the history row
	// are written together or not at all.
	err = withLocalTx(db, func(tx *sql.Tx) error {
		if err := models.UpdateAccountPassword(tx, a.ID, hash); err != nil {
			return err
		}
		if err := models.SetAccountDisabled(tx, a.ID, false); err != nil {
			return err
		}
		if err := RevokeAccountAuth(tx, a.ID, ""); err != nil {
			return err
		}
		detail := M("history.detail.admin_recovered_cli")
		return insertLocalHistory(tx, &models.HistoryItem{AccountName: localOperator(), Category: models.HistAccount, Action: "password_reset", Target: a.Email,
			Detail: detail.Text(), I18n: HistoryTextsJSON(Msg{}, detail)})
	})
	if err != nil {
		return err
	}
	fmt.Printf("Password of %q reset; the account is enabled and its sessions, trusted browsers and pending codes were invalidated.\n", a.Email)
	return nil
}

// AgentCmd groups the agent's commands.
type AgentCmd struct {
	Init       AgentInitCmd       `cmd:"" help:"Initialize the agent (the token is read from standard input, never from an argument)"`
	SetCentral AgentSetCentralCmd `cmd:"" name:"set-central" help:"Change the central host (and port) of the initialized agent; the agent must be stopped"`
	Start      AgentStartCmd      `cmd:"" help:"Start the agent"`
	Stop       AgentStopCmd       `cmd:"" help:"Stop the running agent (sends SIGTERM and waits until it has finished its cycle)"`
	Status     AgentStatusCmd     `cmd:"" help:"Show whether the agent is running and its state (exit code 3 when it is not)"`
}

// AgentInitCmd initializes the agent.
type AgentInitCmd struct {
	CentralHost   string `name:"central-host" help:"Host name or IP address of the central" required:""`
	CentralPort   int    `name:"central-port" help:"Port of the central for the agents" default:"${default_agent_port}"`
	Overwrite     bool   `help:"Replace an existing initialization"`
	CAFingerprint string `name:"ca-fingerprint" help:"Expected SHA-256 fingerprint of the central CA (shown on the setup guide screen)"`
}

func (c *AgentInitCmd) Run() error {
	return AgentInit(AgentInitOptions{
		CentralHost: c.CentralHost, CentralPort: c.CentralPort, Overwrite: c.Overwrite, CAFingerprint: c.CAFingerprint, DataDir: DataDir(),
	})
}

// AgentSetCentralCmd changes the central host of an initialized agent.
type AgentSetCentralCmd struct {
	Host string `help:"New host name or IP address of the central" required:""`
	Port *int   `help:"Port of the central for the agents (default: keep the current port)"`
}

func (c *AgentSetCentralCmd) Run() error {
	port := 0 // keep the stored port
	if c.Port != nil {
		if *c.Port < 1 || *c.Port > 65535 {
			return NewExitErrorf(ExitArgument, "central port %d must be 1-65535", *c.Port)
		}
		port = *c.Port
	}
	return AgentSetCentral(DataDir(), c.Host, port)
}

// AgentStartCmd starts the agent.
type AgentStartCmd struct{}

func (c *AgentStartCmd) Run() error {
	return AgentStart(DataDir())
}
