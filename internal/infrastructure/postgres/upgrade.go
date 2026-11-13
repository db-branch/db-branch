package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

var _ domain.Upgrader = (*Server)(nil)

// CheckUpgrade confirms that the data directory belongs to the major version
// of the new runtime: a minor upgrade swaps the binaries and nothing else.
func (p *Server) CheckUpgrade(_ context.Context, s domain.Server, to domain.RuntimeRef, _ domain.Login) (domain.UpgradePlan, error) {
	b, err := os.ReadFile(filepath.Join(s.DataDir, "PG_VERSION"))
	if err != nil {
		return domain.UpgradePlan{}, fmt.Errorf("read PG_VERSION: %w", err)
	}
	have, want := strings.TrimSpace(string(b)), strings.SplitN(to.Version, ".", 2)[0]
	if have != want {
		return domain.UpgradePlan{Findings: []domain.Finding{{Level: domain.LevelError, Code: domain.CodeUpgradePath,
			Message: fmt.Sprintf("The data directory is PostgreSQL %s; %s cannot open it (major upgrades are not supported yet).", have, to)}}}, nil
	}
	return domain.UpgradePlan{Findings: []domain.Finding{{Level: domain.LevelInfo, Code: domain.CodeUpgradePath,
		Message: fmt.Sprintf("Minor upgrade within PostgreSQL %s: the server restarts on the new binaries; the data files are unchanged.", have)}}}, nil
}

// StartUpgraded starts the server on the new binaries.
func (p *Server) StartUpgraded(ctx context.Context, s domain.Server, _ domain.Login) (domain.UpgradeLog, error) {
	err := p.Start(ctx, s)
	return domain.UpgradeLog{Excerpt: p.journal(ctx, s)}, err
}
