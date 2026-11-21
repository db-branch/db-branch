package domain

import (
	"net"
	"net/url"
	"strconv"
)

// DefaultRuntimes is the registry the process works with. It starts with the
// built-in engines (MySQL, PostgreSQL); further engines must be registered
// before anything is decoded (stored state names runtimes, and
// RuntimeRef.UnmarshalText refuses an engine it does not know).
var DefaultRuntimes = NewBuiltinRuntimes()

// NewBuiltinRuntimes returns a new registry holding only MySQL and PostgreSQL.
func NewBuiltinRuntimes() *RuntimeRegistry {
	r := NewRuntimeRegistry()
	for _, s := range []EngineSpec{mysqlSpec(), postgresSpec()} {
		if err := r.Register(s); err != nil {
			panic(err) // the built-in declarations are fixed: a mistake shows in every test
		}
	}
	return r
}

// The datasets of both engines inherit these from <pool>/db-branch: the
// 16K InnoDB page (also better than 8K for PostgreSQL's compression).
func builtinZFSProps() map[string]string {
	return map[string]string{"recordsize": "16K", "compression": "lz4"}
}

func mysqlSpec() EngineSpec {
	return EngineSpec{
		Engine:      MySQL,
		DisplayName: "MySQL",
		ZFSProps:    builtinZFSProps(),
		Protocols:   []ProtocolPort{{Name: "mysql", Primary: true}},
		Capabilities: RuntimeCaps{
			Query: true, Checkpoints: true, LiveFreeze: false, Upgrade: true,
			SourceDump: true, BranchMerge: false, HistoryRecord: true,
			UpdateStats: true,
		},
		CheckUpgradePath: checkMySQLPath,
	}
}

func postgresSpec() EngineSpec {
	return EngineSpec{
		Engine:         Postgres,
		DisplayName:    "PostgreSQL",
		CollationBound: true,
		ZFSProps:       builtinZFSProps(),
		Protocols:      []ProtocolPort{{Name: "postgres", Primary: true}},
		Capabilities: RuntimeCaps{
			Query: true, Checkpoints: true, Upgrade: true,
			SourceDump: true, UpdateStats: true,
		},
		CheckUpgradePath: checkPostgresPath,
		// PostgreSQL clients land in the maintenance database "postgres".
		FormatDSN: func(host string, port int, user, password string) string {
			return formatDSN(Postgres, host, port, user, password, "/postgres")
		},
	}
}

func formatDSN(e Engine, host string, port int, user, password, path string) string {
	u := url.URL{
		Scheme: string(e),
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   path,
	}
	return u.String()
}
