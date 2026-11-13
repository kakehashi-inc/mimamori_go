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
	DefaultRememberDays       = 30
	DefaultTrustDays          = 30
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
	// ChallengePrefix marks the opaque login challenge handed to the browser
	// with a mailed login code, and VerifyPrefix the value of the cookie that
	// remembers a browser in which a login code was entered.
	ChallengePrefix = "mmk_"
	VerifyPrefix    = "mmv_"
	// AuthCodeDigits is the number of digits of a mailed verification code.
	AuthCodeDigits = 6
	// SecretRandomBytes is the number of random bytes in every secret value.
	SecretRandomBytes = 32
)

// --- Server ---

const (
	// The connection server (app/workers/connserver.go) reads requests and
	// writes responses in its accepting flow without waiting; a worker only
	// runs the handler on a request that is complete in memory.
	//
	// ConnTotalTimeout bounds one whole request, counted from the end of the
	// request head: a body larger than MaxHeadBytes must arrive and the
	// response must be written within it. (The head, and a body up to
	// MaxHeadBytes, must arrive within the queue timeout.)
	ConnTotalTimeout = 60 * time.Second
	// TLSHandshakeTimeout bounds a TLS handshake, counted from its start.
	// A handshake starts when the client's first record (the ClientHello)
	// is complete and it is the connection's turn (one at a time), so only
	// the client's next flight is waited for: one round trip and its key
	// exchange. While it waits, the accepting flow does all its other work.
	TLSHandshakeTimeout = 2 * time.Second
	// HandshakeGrace is how long a handshake may wait for its client while
	// another ClientHello waits for its turn (counted from the start of the
	// handshake): one round trip and the key exchange of an honest client,
	// even across continents. It then gives way, and its source counts as
	// stalled.
	HandshakeGrace = 400 * time.Millisecond
	// HandshakePenalty is how long the TLS listener takes the connections of
	// a source whose handshake gave up after those of other sources. A
	// source whose handshake ran into TLSHandshakeTimeout is also refused
	// for that time, unless it is this host (loopback or one of its
	// addresses).
	HandshakePenalty = 60 * time.Second
	// EvictGrace is how long a connection must wait on its peer before a
	// full table (MaxPendingConns) drops it for a new one; until then new
	// connections wait in the kernel's backlog.
	EvictGrace = time.Second
	// DrainTimeout and DrainMaxBytes bound how long and how much the central
	// reads and discards after answering a request it did not read
	// completely (an early error or a body it refused), so that the client
	// gets the answer instead of a reset. DrainTimeout also bounds writing
	// such an answer.
	DrainTimeout  = 5 * time.Second
	DrainMaxBytes = 2 * MaxRequestBody
	// MaxHeadBytes bounds a request head (larger: 431). Every connection may
	// always buffer this much; bodies beyond it are read only while all
	// connections together buffer less than MaxBufferedBytes.
	MaxHeadBytes     = 64 << 10
	MaxBufferedBytes = 128 << 20
	// MaxRequestBody limits a request body (agent reports are the largest).
	MaxRequestBody = 32 << 20
	// LoopWakeInterval is the longest the accepting flow waits for socket
	// readiness before it looks at the stop signal and the periodic tasks.
	LoopWakeInterval = 250 * time.Millisecond
	// StopFlushTimeout bounds how long a stopping central finishes the
	// requests in work and writes their responses.
	StopFlushTimeout = 5 * time.Second
	// MaxPendingConns bounds the open connections of the central.
	MaxPendingConns = 256
	// PeriodicTick is the interval of the central periodic tasks.
	PeriodicTick = 30 * time.Second
	// LoginMaxFailures and LoginLockDuration implement the login lockout.
	LoginMaxFailures  = 5
	LoginLockDuration = 15 * time.Minute
	// SessionIdleTimeout ends a login session (without "keep me logged in")
	// this long after the last operation. It is deliberately not a
	// configuration item and is never shown to browser users.
	SessionIdleTimeout = time.Hour
	// VerifiedBrowserTTL is how long a login code entered without "trust this
	// browser" spares new logins from the same browser a code. Like
	// SessionIdleTimeout it is never shown to browser users.
	VerifiedBrowserTTL = 3 * time.Hour
	// StepUpValidity is how long a re-authentication (a mailed code entered
	// in a login session) allows changes to command sets from that session.
	StepUpValidity = time.Hour
	// AuthCodeLifetime and AuthCodeMaxAttempts bound a mailed verification
	// code (login, e-mail address change or re-authentication).
	AuthCodeLifetime    = 10 * time.Minute
	AuthCodeMaxAttempts = 5
	// AuthCodeMailTimeoutCap bounds the sending of a code mail inside a
	// request (smtp.timeout_sec applies when shorter), so that the response
	// is written within ConnTotalTimeout.
	AuthCodeMailTimeoutCap = 45 * time.Second
	// MaxTrustDays is the largest two_step_trust_days.
	MaxTrustDays = 365
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

// --- Command sets ---

const (
	// MaxCommandSetNameLength is the largest set name, in characters.
	MaxCommandSetNameLength = 100
	// MaxCommandTextLength bounds a set description and a command note, in
	// characters.
	MaxCommandTextLength = 1000
	// MaxCommandOSUserLength is the largest OS user name, in characters.
	MaxCommandOSUserLength = 256
	// MaxCommandWorkDirBytes is the largest working directory, in bytes.
	MaxCommandWorkDirBytes = 4096
	// MaxCommandLineBytes is the largest command line, in bytes.
	MaxCommandLineBytes = 102400
	// DefaultCommandSetTimeoutSec is the default time limit of one command;
	// MaxCommandSetTimeoutSec the largest. (DefaultCommandTimeoutSec above
	// is the limit of the agent's own external commands such as auditctl.)
	DefaultCommandSetTimeoutSec = 300
	MaxCommandSetTimeoutSec     = 86400
	// DefaultCommandLineLimit is the default number of lines of a command's
	// part of the display value; MaxCommandLineLimit the largest.
	DefaultCommandLineLimit = 20
	MaxCommandLineLimit     = 10000
	// DefaultCommandKeepRuns is the default number of finished runs kept per
	// set; MaxCommandKeepRuns the largest.
	DefaultCommandKeepRuns = 100
	MaxCommandKeepRuns     = 10000
	// MaxCommandExitCode is the largest exit code a code spec can name.
	MaxCommandExitCode = 255
	// MaxCommandOutputBytes is the console output stored per command; what
	// exceeds is read and dropped.
	MaxCommandOutputBytes = 1 << 20
	// MaxCommandValueBytes bounds the extracted values of one command and
	// MaxDisplayValueBytes the values of the display value of one run (what
	// exceeds is dropped and marked truncated), so that a pattern that
	// captures the same text many times cannot make a report larger than
	// the central accepts (MaxRequestBody).
	MaxCommandValueBytes = 1 << 20
	MaxDisplayValueBytes = 1 << 20
	// CommandPatternTimeout bounds the matching of one pattern against the
	// output of one command.
	CommandPatternTimeout = 2 * time.Second
	// CommandKillGrace is the time between SIGTERM and SIGKILL when a
	// command passes its time limit or the agent stops.
	CommandKillGrace = 10 * time.Second
	// CommandRequestExpiry is how long a manual run request waits for
	// delivery before it expires.
	CommandRequestExpiry = 24 * time.Hour
	// MaxOutboxCommandBytes bounds the queued reports of command set runs on
	// the agent (their console output is dropped first, oldest first).
	MaxOutboxCommandBytes = 64 << 20
)
