package runtimeprovider_test

import (
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/mysql"
	"github.com/db-branch/db-branch/internal/infrastructure/postgres"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimeprovider"
	"github.com/db-branch/db-branch/internal/infrastructure/sqlquery"
	"github.com/db-branch/db-branch/internal/infrastructure/srcdump"
)

// The agent wires the built-in providers like this (internal/app): what they
// declare once wired must be what the registry says, or the API would offer
// features (and refuse others) that the engine's provider does differently.
func TestBuiltinProviders_配線した状態の宣言はレジストリと一致する(t *testing.T) {
	queries := sqlquery.Runner{}
	providers := map[domain.Engine]domain.RuntimeProvider{
		domain.MySQL:    mysql.NewProvider(&mysql.Server{}, runtimeprovider.Hooks{Query: queries.QueryHook(), DumpSource: srcdump.MySQLDump(nil)}),
		domain.Postgres: postgres.NewProvider(&postgres.Server{}, runtimeprovider.Hooks{Query: queries.QueryHook(), DumpSource: srcdump.PostgresDump(nil)}),
	}
	for engine, p := range providers {
		got, err := p.Describe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := domain.DefaultRuntimes.Spec(engine)
		if !ok {
			t.Fatalf("%s is not registered", engine)
		}
		if got.Capabilities != want.Capabilities || got.Engine != want.Engine || got.CollationBound != want.CollationBound {
			t.Errorf("%s: provider declares %+v, registry %+v", engine, got, want)
		}
	}
}
