package modules

import "time"

// --- Application ---

const (
	// AppName is the executable name, also used as the CLI name.
	AppName = "mimamori"
	// BuiltinAgentName is the fixed display name of the built-in agent (the
	// central host monitoring itself).
	BuiltinAgentName = "(central)"
	// AuditKey is the auditd rule key attached to every rule this system loads.
	// Only rules carrying this key are added, replaced or removed by the agent.
	AuditKey = "mimamori"
	// AuditRulesFile holds the persistent copy of the generated rules so that
	// they are reloaded by augenrules at boot. It never edits other files.
	// augenrules concatenates rules.d in C-locale name order, and distribution
	// files may be named without a number (Ubuntu: audit.rules), so the name
	// starts with "zz-" to come after every other file and win for the global
	// settings.
	AuditRulesFile = "/etc/audit/rules.d/zz-mimamori.rules"
	// AuditdConfFile is read (never written) to warn about halting actions.
	AuditdConfFile = "/etc/audit/auditd.conf"
)

// --- Paths ---

const (
	// Every file is in the data/ directory next to the executable (or the
	// directory given with --data-dir). The configuration is stored in the
	// databases.
	DataDirName             = "data"
	CentralDBFileName       = "central.db"
	AgentLocalDBFileName    = "agent.db"
	BuiltinAgentLocalDBName = "builtin-agent.db"
	TargetDBDirName         = "targets"
	CentralLockFileName     = "central.lock"
	// CLISecretFileName holds the secret the central creates at start for
	// the management CLI; whoever can read the data directory can use it.
	CLISecretFileName = "cli.secret"
	AgentLockFileName = "agent.lock"
	CABundleFileName  = "ca.crt"
	// Default listeners of the central: the browser screens and the agents.
	// Change the ports here; everything else (listen addresses, the port an
	// agent connects to, examples on the screens) is derived from them.
	DefaultListenAddress      = "0.0.0.0"
	DefaultWebPort            = 9080
	DefaultAgentPort          = 9081
	DefaultCheckIntervalSec   = 60
	DefaultScanIntervalSec    = 3600
	DefaultTokenValidDays     = 365
	DefaultTokenRenewDays     = 30
	DefaultRetentionDays      = 180
	DefaultNotifyIntervalMin  = 30
	DefaultNotifyDedupeHours  = 24
	DefaultNotifyMaxLines     = 200
	DefaultWorkers            = 1
	DefaultQueueTimeoutSec    = 10
	DefaultSessionHours       = 12
	DefaultRememberDays       = 30
	DefaultSMTPTimeoutSec     = 30
	DefaultCommandTimeoutSec  = 60
	DefaultHTTPTimeoutSec     = 30
	DefaultOutboxMaxDetection = 10000
)

// --- Secrets ---

const (
	// TokenPrefix marks agent tokens and SessionPrefix marks browser session ids.
	TokenPrefix   = "mmr_"
	SessionPrefix = "mms_"
	// SecretRandomBytes is the number of random bytes in every secret value.
	SecretRandomBytes = 32
)

// --- Server ---

const (
	// ConnHeaderTimeout bounds TLS handshake and request header reading.
	ConnHeaderTimeout = 10 * time.Second
	// ConnTotalTimeout bounds one whole request (read, handle, write).
	ConnTotalTimeout = 60 * time.Second
	// MaxRequestBody limits a request body (agent reports are the largest).
	MaxRequestBody = 32 << 20
	// AcceptPollInterval is how long one listener is polled before the next.
	AcceptPollInterval = 20 * time.Millisecond
	// MaxPendingConns bounds connections accepted but not yet handled.
	MaxPendingConns = 256
	// PeriodicTick is the interval of the central periodic tasks.
	PeriodicTick = 30 * time.Second
	// LoginMaxFailures and LoginLockDuration implement the login lockout.
	LoginMaxFailures  = 5
	LoginLockDuration = 15 * time.Minute
	// MinPasswordLength is the minimum length of account passwords.
	MinPasswordLength = 10
	// PasswordIterations is the PBKDF2-SHA256 iteration count.
	PasswordIterations = 600000
)

// --- Certificates ---

const (
	CAValidity            = 5 * 365 * 24 * time.Hour
	CARotateBefore        = 180 * 24 * time.Hour // create the next CA this long before expiry
	CASwitchAfter         = 30 * 24 * time.Hour  // switch the server certificate this long after
	ServerCertValidity    = 397 * 24 * time.Hour
	ServerCertRenewBefore = 30 * 24 * time.Hour
)

// --- Rules ---

const (
	MaxRuleElements  = 200
	MaxExtensions    = 50
	MinCheckInterval = 10
	MaxCheckInterval = 3600
	MinScanInterval  = 60
	MaxScanInterval  = 7 * 24 * 3600
	MaxTargetLength  = 512
	// AuditBacklogLimit is the backlog size the agent sets (-b).
	AuditBacklogLimit = 8192
	// MaxScanEntries bounds the number of entries one scan visits.
	MaxScanEntries = 2000000
	// MaxDirectoryEntries bounds the reported directory structure.
	MaxDirectoryEntries = 300
	// DirectoryDepth is the depth of the reported directory structure.
	DirectoryDepth = 3
)
