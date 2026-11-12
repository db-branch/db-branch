package domain

import (
	"context"
	"io"
)

// Snapshot is a point-in-time, read-only view of a dataset.
type Snapshot struct {
	Dataset string
	Name    string
}

func (s Snapshot) String() string { return s.Dataset + "@" + s.Name }

// DatasetInfo describes a dataset's mount point and space usage in bytes.
type DatasetInfo struct {
	Mountpoint string
	// Used is the space unique to this dataset (for a clone: its own changes).
	Used uint64
	// Referenced is the total data the dataset can see, including data shared with its origin.
	Referenced uint64
}

// Storage is the copy-on-write storage that backs bases and branches.
type Storage interface {
	Create(ctx context.Context, dataset string, props map[string]string) error
	Snapshot(ctx context.Context, s Snapshot) error
	Clone(ctx context.Context, origin Snapshot, dataset string) error
	Rollback(ctx context.Context, s Snapshot) error
	Destroy(ctx context.Context, dataset string) error
	// DestroySnapshot removes one snapshot; it fails while clones depend on it.
	DestroySnapshot(ctx context.Context, s Snapshot) error
	Exists(ctx context.Context, dataset string) (bool, error)
	Info(ctx context.Context, dataset string) (DatasetInfo, error)
}

// SharedStorage is storage every node sees (e.g. Amazon FSx for OpenZFS
// over NFS): bases need no copies on other nodes, and a branch's data stays
// where it is when its server moves.
type SharedStorage interface {
	// Mounted lists the datasets under parent that this node has mounted,
	// by their last path component.
	Mounted(ctx context.Context, parent string) ([]string, error)
	// Release unmounts dataset from this node without destroying it.
	Release(ctx context.Context, dataset string) error
}

// SnapshotStreams copies snapshots between nodes (zfs send / recv).
type SnapshotStreams interface {
	// Send writes every snapshot of dataset after from (all when from is
	// empty) up to and including to.
	Send(ctx context.Context, dataset, from, to string, w io.Writer) error
	// Receive creates dataset, or advances it, from a Send stream.
	Receive(ctx context.Context, dataset string, r io.Reader) error
	// Children lists the direct children of parent by their last path component.
	Children(ctx context.Context, parent string) ([]string, error)
	// Snapshots lists the snapshot names of dataset, oldest first.
	Snapshots(ctx context.Context, dataset string) ([]string, error)
}

// SnapshotSource fetches a base's snapshot stream from the node holding it.
type SnapshotSource interface {
	Open(ctx context.Context, node string, base Name, from, to string) (io.ReadCloser, error)
}
