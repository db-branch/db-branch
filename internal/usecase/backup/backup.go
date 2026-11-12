// Package backup saves the cluster state (configuration and records) on
// each node at an interval, keeps the newest ones and restores one.
package backup

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/db-branch/db-branch/internal/domain"
)

// Source writes and restores the cluster state.
type Source interface {
	WriteBackup(w io.Writer, node string, now time.Time) error
	RestoreBackup(ctx context.Context, r io.Reader) (domain.RestoreSummary, error)
}

// Dir is where a node keeps its backups.
type Dir interface {
	// Create starts a file that appears under name only once closed.
	Create(name string) (io.WriteCloser, error)
	// List returns the backups, newest first.
	List() ([]domain.BackupFile, error)
	Remove(name string) error
	Open(name string) (io.ReadCloser, error)
}

// Service takes, lists and restores backups.
type Service struct {
	Source Source
	Dir    Dir
	Node   string
	Clock  func() time.Time
	// Keep is how many backups to keep; Every how often to take one (0: never).
	Keep  func() int
	Every func() time.Duration
}

var namePattern = regexp.MustCompile(`^db-branch-[a-z0-9.-]+-\d{8}T\d{6}Z\.json\.gz$`)

// Take saves the state now and removes the oldest backups beyond Keep.
func (s *Service) Take(ctx context.Context) (domain.BackupFile, error) {
	now := s.Clock().UTC()
	name := fmt.Sprintf("db-branch-%s-%s.json.gz", s.Node, now.Format("20060102T150405Z"))
	w, err := s.Dir.Create(name)
	if err != nil {
		return domain.BackupFile{}, err
	}
	if err := s.Source.WriteBackup(w, s.Node, now); err != nil {
		_ = w.Close()
		_ = s.Dir.Remove(name)
		return domain.BackupFile{}, err
	}
	if err := w.Close(); err != nil {
		return domain.BackupFile{}, err
	}
	list, err := s.Dir.List()
	if err != nil {
		return domain.BackupFile{}, err
	}
	for i, f := range list {
		if i >= s.Keep() && s.Keep() > 0 {
			_ = s.Dir.Remove(f.Name)
		}
	}
	for _, f := range list {
		if f.Name == name {
			return f, nil
		}
	}
	return domain.BackupFile{Name: name, TakenAt: now}, nil
}

// List returns this node's backups, newest first.
func (s *Service) List() ([]domain.BackupFile, error) { return s.Dir.List() }

// Due reports whether the interval has passed since the newest backup.
func (s *Service) Due() bool {
	every := s.Every()
	if every <= 0 {
		return false
	}
	list, err := s.Dir.List()
	if err != nil || len(list) == 0 {
		return err == nil
	}
	return !s.Clock().Before(list[0].TakenAt.Add(every))
}

// Open reads one of this node's backups by name.
func (s *Service) Open(name string) (io.ReadCloser, error) {
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("backup %q: %w", name, domain.ErrNotFound)
	}
	return s.Dir.Open(name)
}

// Restore applies a backup to the cluster.
func (s *Service) Restore(ctx context.Context, r io.Reader) (domain.RestoreSummary, error) {
	return s.Source.RestoreBackup(ctx, r)
}
