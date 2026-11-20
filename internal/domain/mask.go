package domain

import "context"

// AdminAccess is how a tool on this node reaches a database server as its
// local admin: over the local socket, without a password.
type AdminAccess struct {
	Engine Engine `json:"engine"`
	// Host and Port reach it over TCP (for tools without socket support,
	// which need a login of their own).
	Host string `json:"host"`
	// Socket is the socket directory (PostgreSQL) or file (MySQL).
	Socket string `json:"socket"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	// BinDir holds the runtime's client tools (pg_dump, mysqldump, ...),
	// which need LibDir on LD_LIBRARY_PATH.
	BinDir string `json:"bin_dir"`
	LibDir string `json:"lib_dir"`
	// Endpoints are the instance's addresses by protocol name, for an engine
	// with several ports; the fields above are those of its primary
	// protocol. Env is what a client tool of the engine needs in its environment.
	Endpoints map[string]Endpoint `json:"endpoints,omitempty"`
	Env       map[string]string   `json:"env,omitempty"`
}

// Endpoint is where one protocol of an instance listens.
type Endpoint struct {
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Socket string `json:"socket,omitempty"`
}

// MaskJob asks a masker to read Source and write a masked copy into Target
// (a base's server, of the same engine) by Rules (in the masker's format).
type MaskJob struct {
	Source, Target AdminAccess
	Rules          string
}

// Masker writes masked copies of databases (e.g. with Greenmask).
type Masker interface {
	Mask(ctx context.Context, job MaskJob) (log string, err error)
}

// MaskPlan describes a mask run to a MaskPolicy.
type MaskPlan struct {
	Source         Name
	SourceSnapshot string
	Target         Name
	Masker         string
	Rules          string
	// By is who started it.
	By                 string
	SourceDB, TargetDB AdminAccess
}

// MaskPolicy governs mask runs (a paid module): it may refuse a run or
// replace its rules with approved ones, check the result before it is
// kept, and record the snapshot it became.
type MaskPolicy interface {
	PrepareMask(ctx context.Context, p MaskPlan) (rules string, err error)
	VerifyMask(ctx context.Context, p MaskPlan) error
	MaskRecorded(ctx context.Context, p MaskPlan, snapshot string) error
}

// MaskRun names a mask run to start after a refresh; the policy supplies
// the rules as for any run.
type MaskRun struct {
	Target, Masker string
}

// RefreshFollower says which mask runs follow a base's refresh (a paid
// module). Without one, a refresh is followed by nothing.
type RefreshFollower interface {
	AfterRefresh(ctx context.Context, base Name, snapshot string) ([]MaskRun, error)
}

// MaskInfo records how a base snapshot was masked.
type MaskInfo struct {
	Source         Name   `json:"source"`
	SourceSnapshot string `json:"source_snapshot"`
	Masker         string `json:"masker"`
	// RulesSHA256 identifies the rules used.
	RulesSHA256 string `json:"rules_sha256"`
	By          string `json:"by,omitempty"`
}
