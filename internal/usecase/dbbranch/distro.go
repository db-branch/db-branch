package dbbranch

import "github.com/db-branch/db-branch/internal/domain"

// ownDistro is the distribution a new base of engine would record: this
// node's, for engines whose data depends on it (domain.CollationBound).
func (s *Service) ownDistro(e domain.Engine) string {
	if !domain.CollationBound(e) {
		return ""
	}
	return s.Distro
}

// originDistro is the distribution a branch of snap must run on, or "" when
// the engine does not care. It is the one recorded on the snapshot, else (for
// snapshots made before it was recorded) the one the base's node reports now.
func (s *Service) originDistro(base domain.Base, snap domain.BaseSnapshot, nodes []domain.Node) string {
	if !domain.CollationBound(base.Runtime.Engine) {
		return ""
	}
	if snap.Distro != "" {
		return snap.Distro
	}
	if base.Node == "" || base.Node == s.Node && s.Distro != "" {
		return s.Distro
	}
	for _, n := range nodes {
		if n.Name == base.Node {
			return n.System.Distro
		}
	}
	return ""
}
