package rtmodule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// InstallRequest describes a runtime module install or upgrade operation.
type InstallRequest struct {
	Ref       domain.GitHubRef
	Upgrade   bool
	NoRestart bool
	TrustKey  domain.Fingerprint
}

// InstallResult summarizes the outcome of Install.
type InstallResult struct {
	Engine          string
	Version         string
	PreviousVersion string
	Tag             string
	KeyID           string
	KeyFingerprint  domain.Fingerprint
	PreviousKeyID   string
	Upgraded        bool
	User            string
	UserCreated     bool
	UserUID         uint32
	UserGID         uint32
	KeyAdded        bool
	SourceMissing   bool
	Installed       bool
	Restarted       bool
	RestartFailed   bool
	Warnings        []string
}

// Service orchestrates local runtime module management through ports.
type Service struct {
	Releases  ReleaseSource
	Extractor ArchiveExtractor
	Keys      KeyStore
	Verifier  Verifier
	Store     ModuleStore
	Users     SystemUsers
	Usage     UsageChecker
	Prompter  TrustPrompter
	Restarter AgentRestarter

	// Arch selects the target Linux architecture ("amd64" or "arm64"; defaults to runtime.GOARCH).
	Arch string
	// MaxDownloadBytes caps a downloaded release asset (defaults to DefaultMaxDownloadBytes).
	MaxDownloadBytes int64
	// CreateTemp creates a temporary file for downloading an archive (defaults to os.CreateTemp).
	CreateTemp func() (TempFile, error)
	// RemoveFile removes a temporary download file (defaults to os.Remove).
	RemoveFile func(path string) error
}

func (s *Service) arch() (string, error) {
	a := s.Arch
	if a == "" {
		a = runtime.GOARCH
	}
	if a != "amd64" && a != "arm64" {
		return "", fmt.Errorf("%w: unsupported architecture %q (want amd64 or arm64)", domain.ErrInvalidRef, a)
	}
	return a, nil
}

func (s *Service) maxDownloadBytes() int64 {
	if s.MaxDownloadBytes > 0 {
		return s.MaxDownloadBytes
	}
	return DefaultMaxDownloadBytes
}

func (s *Service) createTemp() (TempFile, error) {
	if s.CreateTemp != nil {
		return s.CreateTemp()
	}
	return os.CreateTemp("", "db-branch-rtmod-*.tar.gz")
}

func (s *Service) removeFile(path string) {
	if path == "" {
		return
	}
	if s.RemoveFile != nil {
		_ = s.RemoveFile(path)
		return
	}
	_ = os.Remove(path)
}

func wrapBadSignature(err error) error {
	if errors.Is(err, ErrBadSignature) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrBadSignature, err)
}

// Install downloads, verifies, and atomically installs or upgrades a runtime module.
func (s *Service) Install(ctx context.Context, req InstallRequest) (InstallResult, error) {
	if _, err := domain.ParseGitHubRef(req.Ref.String()); err != nil {
		return InstallResult{}, err
	}
	arch, err := s.arch()
	if err != nil {
		return InstallResult{}, err
	}

	// 1. Lock the module store for the entire install lifecycle.
	unlock, err := s.Store.Lock(ctx)
	if err != nil {
		return InstallResult{}, err
	}
	defer unlock()

	var (
		addedKeyID string
		committed  bool
	)
	defer func() {
		if !committed && addedKeyID != "" {
			_ = s.Keys.Remove(addedKeyID)
		}
	}()

	// 2. Fetch release and select the single matching asset for arch.
	rel, err := s.Releases.GetRelease(ctx, req.Ref)
	if err != nil {
		return InstallResult{}, err
	}
	chosen, parsedAsset, err := selectReleaseAsset(rel, req.Ref.Tag, arch)
	if err != nil {
		return InstallResult{}, err
	}

	// 3. Download asset into a temporary file and extract to staging.
	tmpPath, err := s.downloadToTemp(ctx, chosen)
	if err != nil {
		return InstallResult{}, err
	}
	defer s.removeFile(tmpPath)

	extracted, err := s.Extractor.ExtractToStaging(ctx, tmpPath, parsedAsset.Engine)
	if err != nil {
		return InstallResult{}, err
	}
	if extracted.Cleanup != nil {
		defer extracted.Cleanup()
	}

	// 4. Parse signature and verify with trusted key (or approve/register candidate key).
	keyID, keyFP, keyAdded, err := s.verifyAndTrustSignature(ctx, extracted, req, rel.Tag)
	if err != nil {
		return InstallResult{}, err
	}
	if keyAdded {
		addedKeyID = keyID
	}

	// 5. Verify staged directory in strict mode and enforce manifest contract.
	verified, err := s.Verifier.VerifyStaging(extracted.StageDir, parsedAsset.Engine)
	if err != nil {
		return InstallResult{}, err
	}
	if err := verifyManifestContract(verified, extracted.ManifestRaw, parsedAsset, rel.Tag, keyID); err != nil {
		return InstallResult{}, err
	}

	// 6. Check existing module state, upgrade monotonicity, and provenance.
	engine := verified.Manifest.Engine
	canonicalRepo := domain.GitHubRef{Owner: req.Ref.Owner, Repo: req.Ref.Repo}.String()
	upgradeInfo, err := s.checkExistingModule(engine, verified.Manifest.ModuleVersion, canonicalRepo, keyID, req.Upgrade)
	if err != nil {
		return InstallResult{}, err
	}

	// 7. Ensure isolated OS system user exists.
	userCreated, uid, gid, err := s.Users.EnsureSystemUser(ctx, verified.Manifest.RunAsUser)
	if err != nil {
		return InstallResult{}, err
	}

	// 8. Write provenance metadata and atomically commit.
	if extracted.WriteSource == nil {
		return InstallResult{}, errors.New("extracted module missing WriteSource callback")
	}
	if err := extracted.WriteSource(ModuleSource{
		Repo:  canonicalRepo,
		Tag:   rel.Tag,
		KeyID: keyID,
	}); err != nil {
		return InstallResult{}, err
	}
	if err := s.Store.Commit(extracted.StageDir, engine, upgradeInfo.upgraded); err != nil {
		return InstallResult{}, err
	}
	committed = true

	res := InstallResult{
		Engine:          engine,
		Version:         verified.Manifest.ModuleVersion,
		PreviousVersion: upgradeInfo.previousVersion,
		Tag:             rel.Tag,
		KeyID:           keyID,
		KeyFingerprint:  keyFP,
		PreviousKeyID:   upgradeInfo.previousKeyID,
		Upgraded:        upgradeInfo.upgraded,
		User:            verified.Manifest.RunAsUser,
		UserCreated:     userCreated,
		UserUID:         uid,
		UserGID:         gid,
		KeyAdded:        keyAdded,
		SourceMissing:   upgradeInfo.sourceMissing,
		Installed:       true,
		Warnings:        upgradeInfo.warnings,
	}

	// 9. Restart agent unless suppressed.
	if !req.NoRestart {
		if err := s.Restarter.Restart(ctx); err != nil {
			res.RestartFailed = true
			return res, fmt.Errorf("runtime module %q installed, but restarting agent failed: %w", engine, err)
		}
		res.Restarted = true
	}

	// 10. Cleanup & unlock run via defers.
	return res, nil
}

func selectReleaseAsset(rel Release, requestedTag, arch string) (ReleaseAsset, domain.ModuleAsset, error) {
	if rel.Draft {
		return ReleaseAsset{}, domain.ModuleAsset{}, fmt.Errorf("release %q is a draft", rel.Tag)
	}
	if rel.Prerelease {
		return ReleaseAsset{}, domain.ModuleAsset{}, fmt.Errorf("release %q is a prerelease", rel.Tag)
	}
	if rel.Tag == "" {
		return ReleaseAsset{}, domain.ModuleAsset{}, errors.New("release tag is empty")
	}
	if requestedTag != "" && rel.Tag != requestedTag {
		return ReleaseAsset{}, domain.ModuleAsset{}, fmt.Errorf("release tag %q does not match requested tag %q", rel.Tag, requestedTag)
	}
	var (
		matchedAsset  ReleaseAsset
		matchedParsed domain.ModuleAsset
		matches       int
	)
	for _, a := range rel.Assets {
		parsed, err := domain.ParseModuleAssetName(a.Name)
		if err != nil || parsed.Arch != arch {
			continue
		}
		matchedAsset = a
		matchedParsed = parsed
		matches++
	}
	if matches != 1 {
		return ReleaseAsset{}, domain.ModuleAsset{}, fmt.Errorf("expected exactly 1 linux/%s runtime module asset in release %q, found %d", arch, rel.Tag, matches)
	}
	tagVersion := strings.TrimPrefix(rel.Tag, "v")
	if matchedParsed.Version != tagVersion {
		return ReleaseAsset{}, domain.ModuleAsset{}, fmt.Errorf("asset version %q does not match release tag %q", matchedParsed.Version, rel.Tag)
	}
	return matchedAsset, matchedParsed, nil
}

type existingModuleCheck struct {
	upgraded        bool
	previousVersion string
	previousKeyID   string
	sourceMissing   bool
	warnings        []string
}

func (s *Service) checkExistingModule(engine, newVersion, canonicalRepo, newKeyID string, upgrade bool) (existingModuleCheck, error) {
	existing, err := s.Store.Get(engine)
	if errors.Is(err, domain.ErrNotFound) {
		if upgrade {
			return existingModuleCheck{}, fmt.Errorf("%w: runtime module %q is not installed", domain.ErrNotFound, engine)
		}
		return existingModuleCheck{}, nil
	}
	if err != nil {
		return existingModuleCheck{}, err
	}
	if !upgrade {
		return existingModuleCheck{}, fmt.Errorf("%w: runtime module %q (%s) is already installed; pass --upgrade to update", domain.ErrAlreadyInstalled, engine, existing.Manifest.ModuleVersion)
	}
	if err := domain.CheckModuleUpgrade(existing.Manifest.ModuleVersion, newVersion); err != nil {
		return existingModuleCheck{}, err
	}

	out := existingModuleCheck{
		upgraded:        true,
		previousVersion: existing.Manifest.ModuleVersion,
	}
	src, srcErr := s.Store.ReadSource(engine)
	switch {
	case srcErr == nil:
		if !strings.EqualFold(src.Repo, canonicalRepo) {
			return existingModuleCheck{}, fmt.Errorf("%w: %w: installed module %q came from %s, refusing upgrade from %s",
				domain.ErrConflict, ErrSourceMismatch, engine, src.Repo, canonicalRepo)
		}
	case errors.Is(srcErr, domain.ErrNotFound):
		// Per design §7 #2: modules installed before .source.json (e.g. .deb) may still be upgraded,
		// recording that provenance was absent.
		out.sourceMissing = true
		out.warnings = append(out.warnings, fmt.Sprintf("existing runtime module %q has no provenance metadata (.source.json); upgrading from %s", engine, canonicalRepo))
	default:
		return existingModuleCheck{}, srcErr
	}

	if existing.KeyID != "" && existing.KeyID != newKeyID {
		out.previousKeyID = existing.KeyID
		out.warnings = append(out.warnings, fmt.Sprintf("signing key changed from %q to %q", existing.KeyID, newKeyID))
	}
	return out, nil
}

func (s *Service) downloadToTemp(ctx context.Context, asset ReleaseAsset) (string, error) {
	f, err := s.createTemp()
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := s.Releases.DownloadAsset(ctx, asset, s.maxDownloadBytes(), f); err != nil {
		_ = f.Close()
		s.removeFile(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		s.removeFile(path)
		return "", err
	}
	return path, nil
}

func verifyManifestContract(verified VerifiedModule, manifestRaw []byte, asset domain.ModuleAsset, tag, keyID string) error {
	if !verified.Signed {
		return fmt.Errorf("%w: module is not signed", ErrBadSignature)
	}
	if verified.KeyID != "" && verified.KeyID != keyID {
		return fmt.Errorf("%w: verified key_id %q does not match signature key_id %q", ErrBadSignature, verified.KeyID, keyID)
	}
	if err := domain.ValidRuntimeModuleEngine(verified.Manifest.Engine); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrInvalidManifest, err)
	}
	if verified.Manifest.Engine != asset.Engine {
		return fmt.Errorf("%w: manifest engine %q does not match asset engine %q", domain.ErrInvalidManifest, verified.Manifest.Engine, asset.Engine)
	}
	wantUser := domain.RuntimeUserPrefix + verified.Manifest.Engine
	if verified.Manifest.RunAsUser != wantUser {
		return fmt.Errorf("%w: run_as_user %q must equal %q", domain.ErrInvalidManifest, verified.Manifest.RunAsUser, wantUser)
	}
	var raw struct {
		RunAsUser string `json:"run_as_user"`
	}
	if err := json.Unmarshal(manifestRaw, &raw); err != nil || raw.RunAsUser != wantUser {
		return fmt.Errorf("%w: manifest JSON run_as_user %q must explicitly equal %q", domain.ErrInvalidManifest, raw.RunAsUser, wantUser)
	}
	tagVersion := strings.TrimPrefix(tag, "v")
	if verified.Manifest.ModuleVersion != asset.Version || verified.Manifest.ModuleVersion != tagVersion {
		return fmt.Errorf("%w: manifest module_version %q does not match asset/tag version %q", domain.ErrInvalidManifest, verified.Manifest.ModuleVersion, asset.Version)
	}
	return nil
}

func (s *Service) verifyAndTrustSignature(
	ctx context.Context,
	extracted *ExtractedModule,
	req InstallRequest,
	tag string,
) (keyID string, fp domain.Fingerprint, keyAdded bool, err error) {
	if len(extracted.SignatureRaw) == 0 {
		return "", domain.Fingerprint{}, false, fmt.Errorf("%w: missing manifest.json.sig", ErrBadSignature)
	}
	keyID, sigBytes, err := s.Verifier.ParseSignature(extracted.SignatureRaw)
	if err != nil {
		return "", domain.Fingerprint{}, false, wrapBadSignature(err)
	}

	tk, err := s.Keys.Get(keyID)
	if err == nil {
		// Registered key: verify strictly with the stored key only.
		// Never consult the bundled candidate key, prompt, or overwrite.
		if err := s.Verifier.VerifyWithKey(tk.PublicKey, extracted.ManifestRaw, sigBytes); err != nil {
			return "", domain.Fingerprint{}, false, wrapBadSignature(err)
		}
		trustedFP := tk.Fingerprint
		if trustedFP.String() == "" {
			if computed, fpErr := domain.NewFingerprint(tk.PublicKey); fpErr == nil {
				trustedFP = computed
			}
		}
		if req.TrustKey.String() != "" && !trustedFP.Equal(req.TrustKey) {
			return "", domain.Fingerprint{}, false, fmt.Errorf("%w: trusted key %q has fingerprint %s, want %s", domain.ErrKeyMismatch, keyID, trustedFP, req.TrustKey)
		}
		return keyID, trustedFP, false, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return "", domain.Fingerprint{}, false, err
	}

	// Unregistered key: require a matching bundled candidate key.
	if len(extracted.CandidateKeyRaw) == 0 || extracted.CandidateKeyID == "" || extracted.CandidateKeyID != keyID {
		return "", domain.Fingerprint{}, false, fmt.Errorf("%w: signing key %q is not trusted and no matching bundled public key was provided", domain.ErrKeyUntrusted, keyID)
	}
	candPub, candFP, err := s.Verifier.ParsePublicKey(extracted.CandidateKeyRaw)
	if err != nil {
		return "", domain.Fingerprint{}, false, wrapBadSignature(err)
	}
	// Verify the signature BEFORE prompting or checking TrustKey so an invalid key is never presented.
	if err := s.Verifier.VerifyWithKey(candPub, extracted.ManifestRaw, sigBytes); err != nil {
		return "", domain.Fingerprint{}, false, wrapBadSignature(err)
	}

	if req.TrustKey.String() != "" {
		if !candFP.Equal(req.TrustKey) {
			return "", domain.Fingerprint{}, false, fmt.Errorf("%w: candidate key %q has fingerprint %s, want %s", domain.ErrKeyMismatch, keyID, candFP, req.TrustKey)
		}
	} else {
		if s.Prompter == nil {
			return "", domain.Fingerprint{}, false, fmt.Errorf("%w: signing key %q (fingerprint %s) is not trusted; pass --trust-key %s", domain.ErrKeyUntrusted, keyID, candFP, candFP)
		}
		ok, pErr := s.Prompter.ConfirmTrust(ctx, keyID, candFP, req.Ref, tag)
		if pErr != nil {
			return "", domain.Fingerprint{}, false, pErr
		}
		if !ok {
			return "", domain.Fingerprint{}, false, fmt.Errorf("%w: signing key %q (fingerprint %s) was not approved", domain.ErrKeyUntrusted, keyID, candFP)
		}
	}

	added, err := s.Keys.Add(keyID, candPub)
	if err != nil {
		return "", domain.Fingerprint{}, false, err
	}
	if added.Fingerprint.String() != "" {
		candFP = added.Fingerprint
	}
	return keyID, candFP, true, nil
}

// ModuleInfo is one locally installed runtime module entry returned by List.
type ModuleInfo struct {
	Engine    string
	Version   string
	Signed    bool
	KeyID     string
	RunAsUser string
	Status    string
	Warning   string
	Err       error
}

// RemoveResult summarizes the outcome of Remove.
type RemoveResult struct {
	Engine        string
	Removed       bool
	Restarted     bool
	RestartFailed bool
	UserKept      string
	KeyIDKept     string
}

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validKeyID(id string) bool {
	return keyIDPattern.MatchString(id) && id != "." && !strings.Contains(id, "..")
}

// List returns all locally installed runtime modules and their verification state.
func (s *Service) List() []ModuleInfo {
	raw := s.Store.List()
	out := make([]ModuleInfo, 0, len(raw))
	for _, m := range raw {
		engine := m.Engine
		if engine == "" {
			engine = m.Manifest.Engine
		}
		status := "ok"
		switch {
		case m.Err != nil:
			status = "error"
		case !m.Signed:
			status = "unsigned"
		}
		out = append(out, ModuleInfo{
			Engine:    engine,
			Version:   m.Manifest.ModuleVersion,
			Signed:    m.Signed,
			KeyID:     m.KeyID,
			RunAsUser: m.Manifest.RunAsUser,
			Status:    status,
			Warning:   m.Warning,
			Err:       m.Err,
		})
	}
	return out
}

// Remove deletes an installed runtime module when no bases or branches use it.
// OS users and trusted signing keys are intentionally left in place.
func (s *Service) Remove(ctx context.Context, engine string, noRestart bool) (RemoveResult, error) {
	if err := domain.ValidRuntimeModuleEngine(engine); err != nil {
		return RemoveResult{}, err
	}
	unlock, err := s.Store.Lock(ctx)
	if err != nil {
		return RemoveResult{}, err
	}
	defer unlock()

	userKept := domain.RuntimeUserPrefix + engine
	var keyIDKept string
	existing, err := s.Store.Get(engine)
	if errors.Is(err, domain.ErrNotFound) {
		return RemoveResult{}, err
	}
	if err == nil {
		if existing.Manifest.RunAsUser != "" {
			userKept = existing.Manifest.RunAsUser
		}
		keyIDKept = existing.KeyID
	} else if src, sErr := s.Store.ReadSource(engine); sErr == nil {
		keyIDKept = src.KeyID
	}

	bases, branches, err := s.Usage.InUse(ctx, domain.Engine(engine))
	if err != nil {
		return RemoveResult{}, err
	}
	if len(bases) > 0 || len(branches) > 0 {
		return RemoveResult{}, formatInUseError(engine, bases, branches)
	}

	if err := s.Store.Remove(engine); err != nil {
		return RemoveResult{}, err
	}

	res := RemoveResult{
		Engine:    engine,
		Removed:   true,
		UserKept:  userKept,
		KeyIDKept: keyIDKept,
	}
	if !noRestart {
		if err := s.Restarter.Restart(ctx); err != nil {
			res.RestartFailed = true
			return res, fmt.Errorf("runtime module %q removed, but restarting agent failed: %w", engine, err)
		}
		res.Restarted = true
	}
	return res, nil
}

func formatInUseError(engine string, bases, branches []string) error {
	var parts []string
	if n := len(bases); n > 0 {
		noun := "base"
		if n != 1 {
			noun = "bases"
		}
		parts = append(parts, fmt.Sprintf("%d %s (%s)", n, noun, strings.Join(bases, ", ")))
	}
	if n := len(branches); n > 0 {
		noun := "branch"
		if n != 1 {
			noun = "branches"
		}
		parts = append(parts, fmt.Sprintf("%d %s (%s)", n, noun, strings.Join(branches, ", ")))
	}
	return fmt.Errorf("%w: cannot remove runtime module %q: engine is in use by %s; delete them first",
		domain.ErrInUse, engine, strings.Join(parts, " and "))
}

// TrustAdd parses pubRaw, optionally verifies it against expected, and registers it under keyID.
func (s *Service) TrustAdd(keyID string, pubRaw []byte, expected domain.Fingerprint) (TrustKey, error) {
	if !validKeyID(keyID) {
		return TrustKey{}, fmt.Errorf("%w: invalid key_id %q", domain.ErrInvalidRef, keyID)
	}
	pub, fp, err := s.Verifier.ParsePublicKey(pubRaw)
	if err != nil {
		return TrustKey{}, err
	}
	if expected.String() != "" && !fp.Equal(expected) {
		return TrustKey{}, fmt.Errorf("%w: key %q has fingerprint %s, want %s", domain.ErrKeyMismatch, keyID, fp, expected)
	}
	return s.Keys.Add(keyID, pub)
}

// TrustList returns all trusted keys from the key store.
func (s *Service) TrustList() ([]TrustKey, error) {
	return s.Keys.List()
}

// TrustRemove deletes keyID unless any installed runtime module is signed by it.
func (s *Service) TrustRemove(ctx context.Context, keyID string) error {
	if !validKeyID(keyID) {
		return fmt.Errorf("%w: invalid key_id %q", domain.ErrInvalidRef, keyID)
	}
	unlock, err := s.Store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	var usedBy []string
	for _, m := range s.Store.List() {
		if m.KeyID == keyID {
			eng := m.Engine
			if eng == "" {
				eng = m.Manifest.Engine
			}
			usedBy = append(usedBy, eng)
		}
	}
	if len(usedBy) > 0 {
		return fmt.Errorf("%w: trusted key %q is used by installed runtime module(s) %s; remove them first with `db-branch module remove <engine>`",
			domain.ErrInUse, keyID, strings.Join(usedBy, ", "))
	}
	return s.Keys.Remove(keyID)
}
