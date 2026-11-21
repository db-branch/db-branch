package runtimemodule

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/db-branch/db-branch/internal/domain"
)

const (
	// DefaultMaxTotalBytes is the maximum uncompressed total payload size extracted from a module archive (256 MiB).
	DefaultMaxTotalBytes int64 = 256 << 20
	// DefaultMaxFileBytes is the maximum uncompressed size of a single file in a module archive (200 MiB).
	DefaultMaxFileBytes int64 = 200 << 20
	// DefaultMaxFiles is the maximum number of entries permitted in a module archive.
	DefaultMaxFiles = 16
	// SourceMetadataFile records module provenance inside the module directory (excluded from manifest signature).
	SourceMetadataFile = ".source.json"
	maxSourceBytes     = 16 << 10 // 16 KiB
)

// ModuleSource records the provenance of a locally installed runtime module in .source.json.
type ModuleSource struct {
	Repo  string `json:"repo"`
	Tag   string `json:"tag"`
	KeyID string `json:"key_id"`
}

func (s ModuleSource) validate() error {
	if s.Repo == "" || s.Tag == "" || !KeyIDValid(s.KeyID) {
		return fmt.Errorf("%w: invalid module source metadata", domain.ErrInvalidRef)
	}
	return nil
}

// ExtractedModule holds the validated contents of a module archive unpacked
// into an isolated 0700 staging directory on the same filesystem as Root.
type ExtractedModule struct {
	StageDir        string
	ManifestRaw     []byte
	SignatureRaw    []byte
	CandidateKeyID  string
	CandidateKeyRaw []byte
	Cleanup         func()

	chown func(name string, uid, gid int) error
}

// ArchiveExtractor safely unpacks runtime module .tar.gz archives into staging directories.
type ArchiveExtractor struct {
	// Root holds engine directories and .staging-* directories (default domain.ManifestRoot).
	Root string
	// MaxTotalBytes limits total uncompressed bytes extracted (default DefaultMaxTotalBytes).
	MaxTotalBytes int64
	// MaxFileBytes limits a single uncompressed file (default DefaultMaxFileBytes).
	MaxFileBytes int64
	// MaxFiles limits the number of tar entries (default DefaultMaxFiles).
	MaxFiles int
	// Owner reports the uid owning a file (default: from the file system); false when unknown.
	Owner func(fs.FileInfo) (uint32, bool)
	// Chown sets ownership on staged files and dirs (default: os.Chown).
	Chown func(name string, uid, gid int) error
	// Rand supplies randomness for staging directory names (default: crypto/rand.Reader).
	Rand io.Reader
}

// RootDir returns Root or domain.ManifestRoot.
func (e ArchiveExtractor) RootDir() string {
	if e.Root != "" {
		return e.Root
	}
	return domain.ManifestRoot
}

func (e ArchiveExtractor) maxTotalBytes() int64 {
	if e.MaxTotalBytes > 0 {
		return e.MaxTotalBytes
	}
	return DefaultMaxTotalBytes
}

func (e ArchiveExtractor) maxFileBytes() int64 {
	if e.MaxFileBytes > 0 {
		return e.MaxFileBytes
	}
	return DefaultMaxFileBytes
}

func (e ArchiveExtractor) maxFiles() int {
	if e.MaxFiles > 0 {
		return e.MaxFiles
	}
	return DefaultMaxFiles
}

type cappedReader struct {
	r       io.Reader
	left    int64
	overErr error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			return 0, c.overErr
		}
		return 0, err
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

func (e ArchiveExtractor) loader() Loader {
	return Loader{Root: e.RootDir(), Owner: e.Owner}
}

func validateTarEntryName(name string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%w: unsafe entry name %q", domain.ErrUnsafeArchive, name)
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return fmt.Errorf("%w: control character in entry name %q", domain.ErrUnsafeArchive, name)
		}
	}
	return nil
}

// ExtractToStaging unpacks archivePath into a 0700 .staging-<rand> directory under RootDir.
// On any error, the staging directory is removed before returning.
func (e ArchiveExtractor) ExtractToStaging(ctx context.Context, archivePath, engine string) (*ExtractedModule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return nil, err
	}
	if err := e.loader().checkTrustDir(e.RootDir()); err != nil {
		return nil, err
	}
	suffix, err := randomHex(e.Rand, 12)
	if err != nil {
		return nil, err
	}
	stageDir := filepath.Join(e.RootDir(), ".staging-"+suffix)
	if err := os.Mkdir(stageDir, 0o700); err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stageDir)
		}
	}()
	if err := applyChown(e.Chown, stageDir); err != nil {
		return nil, err
	}
	if err := os.Chmod(stageDir, 0o700); err != nil {
		return nil, err
	}

	af, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer af.Close()

	gz, err := gzip.NewReader(af)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid gzip: %v", domain.ErrUnsafeArchive, err)
	}
	defer gz.Close()

	maxTotal := e.maxTotalBytes()
	maxFile := e.maxFileBytes()
	maxFiles := e.maxFiles()
	streamCap := maxTotal + int64(maxFiles+2)*1024
	cappedGZ := &cappedReader{
		r:       gz,
		left:    streamCap,
		overErr: fmt.Errorf("%w: decompressed stream exceeds %d bytes", domain.ErrUnsafeArchive, maxTotal),
	}

	tr := tar.NewReader(cappedGZ)
	var (
		seen            = map[string]bool{}
		extracted       []string
		manifestRaw     []byte
		signatureRaw    []byte
		candidateKeyID  string
		candidateKeyRaw []byte
		fileCount       int
		totalBytes      int64
	)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, domain.ErrUnsafeArchive) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: invalid tar: %v", domain.ErrUnsafeArchive, err)
		}
		fileCount++
		if fileCount > maxFiles {
			return nil, fmt.Errorf("%w: archive exceeds %d entries", domain.ErrUnsafeArchive, maxFiles)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%w: non-regular tar entry %q (type %q)", domain.ErrUnsafeArchive, hdr.Name, hdr.Typeflag)
		}
		if hdr.Mode&0o7000 != 0 || hdr.FileInfo().Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			return nil, fmt.Errorf("%w: special mode bits on %q (%04o)", domain.ErrUnsafeArchive, hdr.Name, hdr.Mode)
		}
		name := hdr.Name
		if err := validateTarEntryName(name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("%w: duplicate entry %q", domain.ErrUnsafeArchive, name)
		}
		seen[name] = true

		if hdr.Size < 0 || hdr.Size > maxFile {
			return nil, fmt.Errorf("%w: entry %q exceeds %d bytes", domain.ErrUnsafeArchive, name, maxFile)
		}
		if totalBytes+hdr.Size > maxTotal {
			return nil, fmt.Errorf("%w: archive exceeds %d bytes total", domain.ErrUnsafeArchive, maxTotal)
		}
		entryLimit := min(maxFile, maxTotal-totalBytes)

		if strings.HasSuffix(name, ".pub") {
			keyID := strings.TrimSuffix(name, ".pub")
			if !KeyIDValid(keyID) {
				return nil, fmt.Errorf("%w: invalid bundled key name %q", domain.ErrUnsafeArchive, name)
			}
			if candidateKeyID != "" {
				return nil, fmt.Errorf("%w: multiple public key files in archive", domain.ErrUnsafeArchive)
			}
			keyLimit := min(entryLimit, int64(maxKeyBytes))
			data, err := readBounded(tr, keyLimit, name)
			if err != nil {
				return nil, err
			}
			totalBytes += int64(len(data))
			candidateKeyID = keyID
			candidateKeyRaw = data
			continue
		}

		var fileLimit = entryLimit
		switch name {
		case "manifest.json":
			fileLimit = min(fileLimit, int64(maxManifestBytes))
		case "manifest.json.sig":
			fileLimit = min(fileLimit, int64(maxSigBytes))
		}
		dst := filepath.Join(stageDir, name)
		n, captured, err := extractRegularFile(tr, dst, fileLimit, name == "manifest.json" || name == "manifest.json.sig", e.Chown)
		if err != nil {
			return nil, err
		}
		totalBytes += n
		extracted = append(extracted, name)
		switch name {
		case "manifest.json":
			manifestRaw = captured
		case "manifest.json.sig":
			signatureRaw = captured
		}
	}

	// Drain any remaining gzip padding/stream under the decompressed byte cap to catch trailing gzip bombs.
	if _, err := io.Copy(io.Discard, cappedGZ); err != nil {
		return nil, err
	}

	if len(manifestRaw) == 0 {
		return nil, fmt.Errorf("%w: missing manifest.json", domain.ErrUnsafeArchive)
	}
	manifest, err := domain.ParseManifestIn(e.RootDir(), manifestRaw)
	if err != nil {
		return nil, err
	}
	if manifest.Engine != engine {
		return nil, fmt.Errorf("%w: manifest engine %q does not match %q", domain.ErrInvalidManifest, manifest.Engine, engine)
	}
	binName := filepath.Base(manifest.Binary)
	if binName == "manifest.json" || binName == "manifest.json.sig" || binName == "runtimes.json" {
		return nil, fmt.Errorf("%w: binary name %q collides with reserved file", domain.ErrUnsafeArchive, binName)
	}
	allowed := map[string]bool{
		"manifest.json":     true,
		"manifest.json.sig": true,
		"runtimes.json":     true,
		binName:             true,
	}
	hasBin := false
	for _, name := range extracted {
		if !allowed[name] {
			return nil, fmt.Errorf("%w: unexpected file %q in archive", domain.ErrUnsafeArchive, name)
		}
		if name == binName {
			hasBin = true
		}
	}
	if !hasBin {
		return nil, fmt.Errorf("%w: missing binary %q", domain.ErrUnsafeArchive, binName)
	}
	if err := os.Chmod(filepath.Join(stageDir, binName), 0o755); err != nil {
		return nil, err
	}
	if err := syncDir(stageDir); err != nil {
		return nil, err
	}

	keep = true
	var once sync.Once
	cleanup := func() {
		once.Do(func() { _ = os.RemoveAll(stageDir) })
	}
	return &ExtractedModule{
		StageDir:        stageDir,
		ManifestRaw:     manifestRaw,
		SignatureRaw:    signatureRaw,
		CandidateKeyID:  candidateKeyID,
		CandidateKeyRaw: candidateKeyRaw,
		Cleanup:         cleanup,
		chown:           e.Chown,
	}, nil
}

func readBounded(r io.Reader, maxBytes int64, name string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", domain.ErrUnsafeArchive, name, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: entry %q exceeds %d bytes", domain.ErrUnsafeArchive, name, maxBytes)
	}
	return data, nil
}

func extractRegularFile(r io.Reader, dst string, maxBytes int64, capture bool, chown func(string, int, int) error) (int64, []byte, error) {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: create %s: %v", domain.ErrUnsafeArchive, filepath.Base(dst), err)
	}
	var (
		buf bytes.Buffer
		w   io.Writer = f
	)
	if capture {
		w = io.MultiWriter(f, &buf)
	}
	n, copyErr := io.Copy(w, io.LimitReader(r, maxBytes+1))
	if copyErr == nil && n > maxBytes {
		copyErr = fmt.Errorf("%w: entry %q exceeds %d bytes", domain.ErrUnsafeArchive, filepath.Base(dst), maxBytes)
	}
	var cErr, mErr, sErr error
	if copyErr == nil {
		cErr = applyChown(chown, dst)
	}
	if copyErr == nil && cErr == nil {
		mErr = f.Chmod(0o644)
	}
	if copyErr == nil && cErr == nil && mErr == nil {
		sErr = f.Sync()
	}
	closeErr := f.Close()
	for _, err := range []error{copyErr, cErr, mErr, sErr, closeErr} {
		if err != nil {
			return 0, nil, err
		}
	}
	return n, buf.Bytes(), nil
}

// WriteSource writes .source.json inside StageDir with root:root ownership and 0644 mode.
func (m *ExtractedModule) WriteSource(src ModuleSource) error {
	if m == nil || m.StageDir == "" {
		return fmt.Errorf("%w: empty staging directory", domain.ErrInvalidRef)
	}
	if err := src.validate(); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(src, "", "  ")
	data = append(data, '\n')
	dst := filepath.Join(m.StageDir, SourceMetadataFile)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := writeAndSyncFile(f, dst, data, 0o644, m.chown); err != nil {
		return err
	}
	return syncDir(m.StageDir)
}

// ReadModuleSource reads and validates .source.json from dir using Loader's trust checks.
// Returns domain.ErrNotFound when .source.json does not exist.
func (l Loader) ReadModuleSource(dir string) (ModuleSource, error) {
	if err := l.checkTrustDir(dir); err != nil {
		return ModuleSource{}, err
	}
	path := filepath.Join(dir, SourceMetadataFile)
	data, err := l.readTrustedFile(path, maxSourceBytes, ErrUntrusted)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ModuleSource{}, fmt.Errorf("%w: %s", domain.ErrNotFound, path)
		}
		return ModuleSource{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var src ModuleSource
	if err := dec.Decode(&src); err != nil {
		return ModuleSource{}, fmt.Errorf("%w: %v", domain.ErrInvalidRef, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return ModuleSource{}, fmt.Errorf("%w: trailing data in %s", domain.ErrInvalidRef, SourceMetadataFile)
	}
	if err := src.validate(); err != nil {
		return ModuleSource{}, err
	}
	return src, nil
}
