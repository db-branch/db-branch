package domain

import (
	"errors"
	"io"
	"time"
)

var (
	// ErrTooLarge rejects an upload over the size limit.
	ErrTooLarge = errors.New("too large")
	// ErrNoSpace rejects an upload the node's disk cannot hold.
	ErrNoSpace = errors.New("not enough disk space")
)

// UploadRef is how a request names an uploaded dump instead of a path.
const UploadRef = "upload:"

// Upload is a dump file sent to a node, to create or load a base from.
type Upload struct {
	// ID is used as "upload:<ID>" in place of a dump path.
	ID        string
	Name      string
	Size      int64
	Path      string
	CreatedAt time.Time
}

// UploadStore keeps uploaded dumps on this node until they are used.
type UploadStore interface {
	// Save streams exactly size bytes of a dump named name (.sql or .sql.gz).
	Save(r io.Reader, name string, size int64) (Upload, error)
	// Get finds an upload by ID.
	Get(id string) (Upload, error)
	Remove(id string) error
	// Room is the largest upload accepted now (size limit and disk space).
	Room() (int64, error)
}
