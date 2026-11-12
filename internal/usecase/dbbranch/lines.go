package dbbranch

import (
	"context"
	"fmt"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
)

// lineMounts remembers the directories OpenLine handed out, so CloseLine
// unmounts only those. A value of false is a directory that was not mounted
// for the caller (the live dataset's own).
type lineMounts struct {
	mu   sync.Mutex
	dirs map[string]bool
}

func (m *lineMounts) add(dir string, mounted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dirs == nil {
		m.dirs = map[string]bool{}
	}
	m.dirs[dir] = mounted
}

func (m *lineMounts) take(dir string) (mounted, known bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mounted, known = m.dirs[dir]
	delete(m.dirs, dir)
	return mounted, known
}

// BranchAdmin is how a tool on this node reaches the branch's server as its
// local admin.
func (s *Service) BranchAdmin(ctx context.Context, n domain.Name) (domain.AdminAccess, error) {
	br, err := s.ownBranch(ctx, n)
	if err != nil {
		return domain.AdminAccess{}, err
	}
	srv, err := s.server(br.Runtime)
	if err != nil {
		return domain.AdminAccess{}, err
	}
	spec, err := s.spec(ctx, branchServerID(br.Name), s.branchDataset(br.Name), br.Runtime, br.Port)
	if err != nil {
		return domain.AdminAccess{}, err
	}
	return srv.Admin(spec), nil
}

// OpenLine gives read-only access to the data of a line of the branch: the
// live one, or one a restore set aside. With a checkpoint, the data as it
// was when the checkpoint was taken (wherever it lives now). The live line
// is returned as the server's own data directory; others are mounted in a
// directory of their own. Release it with CloseLine.
func (s *Service) OpenLine(ctx context.Context, n domain.Name, line int, checkpoint string) (string, error) {
	if err := s.requireLineage(); err != nil {
		return "", err
	}
	if _, err := s.ownBranch(ctx, n); err != nil {
		return "", err
	}
	lines, err := s.family(ctx, n)
	if err != nil {
		return "", err
	}
	if checkpoint != "" {
		ds, info, ok := domain.FindCheckpoint(lines, checkpoint)
		if !ok {
			return "", fmt.Errorf("checkpoint %s of branch %s: %w", checkpoint, n, domain.ErrNotFound)
		}
		return s.mountLine(ctx, ds+"@"+info.Name)
	}
	for _, l := range lines {
		if l.Pending || lineNumber(l.Props) != line {
			continue
		}
		if l.Live {
			info, err := s.Storage.Info(ctx, l.Dataset)
			if err != nil {
				return "", err
			}
			s.mounts.add(info.Mountpoint, false)
			return info.Mountpoint, nil
		}
		return s.mountLine(ctx, l.Dataset)
	}
	return "", fmt.Errorf("line %d of branch %s: %w", line, n, domain.ErrNotFound)
}

func (s *Service) mountLine(ctx context.Context, target string) (string, error) {
	dir, err := s.Lineage.MountReadOnly(ctx, target)
	if err == nil {
		s.mounts.add(dir, true)
	}
	return dir, err
}

// CloseLine releases a directory OpenLine returned.
func (s *Service) CloseLine(ctx context.Context, dir string) error {
	mounted, known := s.mounts.take(dir)
	if !known {
		return fmt.Errorf("%w: %s was not opened by OpenLine", domain.ErrNotFound, dir)
	}
	if !mounted {
		return nil
	}
	return s.Lineage.Unmount(ctx, dir)
}
