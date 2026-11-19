package dbbranch

import "github.com/db-branch/db-branch/internal/domain"

// engineProps returns the ZFS properties the engine asks for that differ from
// DatasetProperties (which <pool>/db-branch sets and children inherit). It
// returns nil when the engine is not registered or needs no overrides.
func engineProps(rt domain.RuntimeRef) map[string]string {
	spec, ok := domain.DefaultRuntimes.Spec(rt.Engine)
	if !ok {
		return nil
	}
	var diff map[string]string
	for k, v := range spec.ZFSProps {
		if cur, set := DatasetProperties[k]; set && cur == v {
			continue
		}
		if diff == nil {
			diff = map[string]string{}
		}
		diff[k] = v
	}
	return diff
}
