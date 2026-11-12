// Command db-branch-proxysql is the db-branch module that drives ProxySQL on
// this node. The db-branch-proxysql package installs it as
// /usr/lib/db-branch/modules/proxysql; the agent loads it at start and sends
// it the routing (one hostgroup per branch, users, comment rules) whenever
// the cluster changes. Without this module the agent leaves ProxySQL alone.
package main

import (
	"github.com/db-branch/db-branch/internal/infrastructure/proxysql"
	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/plugin"
)

var version = "dev"

func main() {
	r := proxysql.NewModule(&proxysql.Admin{Addr: "127.0.0.1:6032", CredFile: "/var/lib/db-branch/proxysql/admin.json"})
	plugin.Serve(ext.Module{
		Name:         "proxysql",
		Version:      version,
		Router:       r,
		Configurable: r,
		Settings:     proxysql.Settings,
		Ports:        map[string]int{"mysql": 6033, "postgres": 6133},
	})
}
