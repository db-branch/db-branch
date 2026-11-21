package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
	"github.com/db-branch/db-branch/internal/infrastructure/config"
	"github.com/db-branch/db-branch/internal/infrastructure/filestore"
	"github.com/db-branch/db-branch/internal/infrastructure/githubrelease"
	"github.com/db-branch/db-branch/internal/infrastructure/runtimemodule"
	"github.com/db-branch/db-branch/internal/infrastructure/sysuser"
	"github.com/db-branch/db-branch/internal/interface/cli"
	"github.com/db-branch/db-branch/internal/usecase/rtmodule"
)

type runtimeModuleWiring struct {
	rootDir       string
	keysDir       string
	allowUnsigned bool
	releases      rtmodule.ReleaseSource
	usage         rtmodule.UsageChecker
	prompter      rtmodule.TrustPrompter
	restarter     rtmodule.AgentRestarter
}

func newRuntimeModuleService(w runtimeModuleWiring) *rtmodule.Service {
	loader := runtimemodule.Loader{
		Root:          w.rootDir,
		KeysDir:       w.keysDir,
		AllowUnsigned: w.allowUnsigned,
	}
	return &rtmodule.Service{
		Releases:  w.releases,
		Extractor: archiveExtractorAdapter{extractor: runtimemodule.ArchiveExtractor{Root: w.rootDir}},
		Keys:      keyStoreAdapter{store: runtimemodule.KeyStore{Dir: w.keysDir}},
		Verifier:  verifierAdapter{loader: runtimemodule.Loader{Root: w.rootDir, KeysDir: w.keysDir}},
		Store:     moduleStoreAdapter{store: runtimemodule.ModuleStore{Loader: loader}},
		Users:     &sysuser.Users{},
		Usage:     w.usage,
		Prompter:  w.prompter,
		Restarter: w.restarter,
		Arch:      goruntime.GOARCH,
	}
}

// RuntimeModules wires the local runtime module management service for cfg.
func (n Node) RuntimeModules(cfg config.Config, prompter rtmodule.TrustPrompter) *rtmodule.Service {
	return newRuntimeModuleService(runtimeModuleWiring{
		allowUnsigned: cfg.AllowUnsignedRuntimes,
		releases:      githubReleaseAdapter{client: &githubrelease.Client{Resolve: artifact.SystemResolver}},
		usage:         runtimeUsageChecker{cfg: cfg},
		prompter:      prompter,
		restarter:     nodeRestarter{fn: n.Restart},
	})
}

type githubReleaseAdapter struct {
	client *githubrelease.Client
}

func (a githubReleaseAdapter) GetRelease(ctx context.Context, ref domain.GitHubRef) (rtmodule.Release, error) {
	rel, err := a.client.GetRelease(ctx, ref)
	if err != nil {
		return rtmodule.Release{}, err
	}
	assets := make([]rtmodule.ReleaseAsset, len(rel.Assets))
	for i, item := range rel.Assets {
		assets[i] = rtmodule.ReleaseAsset{
			Name:        item.Name,
			DownloadURL: item.DownloadURL,
			Size:        item.Size,
		}
	}
	return rtmodule.Release{
		Tag:        rel.Tag,
		Prerelease: rel.Prerelease,
		Draft:      rel.Draft,
		Assets:     assets,
	}, nil
}

func (a githubReleaseAdapter) DownloadAsset(ctx context.Context, asset rtmodule.ReleaseAsset, maxBytes int64, dst io.Writer) (int64, error) {
	return a.client.DownloadAsset(ctx, githubrelease.Asset{
		Name:        asset.Name,
		DownloadURL: asset.DownloadURL,
		Size:        asset.Size,
	}, maxBytes, dst)
}

type archiveExtractorAdapter struct {
	extractor runtimemodule.ArchiveExtractor
}

func (a archiveExtractorAdapter) ExtractToStaging(ctx context.Context, archivePath, engine string) (*rtmodule.ExtractedModule, error) {
	ext, err := a.extractor.ExtractToStaging(ctx, archivePath, engine)
	if err != nil {
		return nil, err
	}
	return &rtmodule.ExtractedModule{
		StageDir:        ext.StageDir,
		ManifestRaw:     ext.ManifestRaw,
		SignatureRaw:    ext.SignatureRaw,
		CandidateKeyID:  ext.CandidateKeyID,
		CandidateKeyRaw: ext.CandidateKeyRaw,
		WriteSource: func(src rtmodule.ModuleSource) error {
			return ext.WriteSource(runtimemodule.ModuleSource{
				Repo:  src.Repo,
				Tag:   src.Tag,
				KeyID: src.KeyID,
			})
		},
		Cleanup: ext.Cleanup,
	}, nil
}

type keyStoreAdapter struct {
	store runtimemodule.KeyStore
}

func toUsecaseTrustKey(k runtimemodule.TrustKey) rtmodule.TrustKey {
	return rtmodule.TrustKey{
		KeyID:       k.KeyID,
		Fingerprint: k.Fingerprint,
		PublicKey:   k.PublicKey,
		Err:         k.Err,
	}
}

func (a keyStoreAdapter) Get(keyID string) (rtmodule.TrustKey, error) {
	k, err := a.store.Get(keyID)
	return toUsecaseTrustKey(k), err
}

func (a keyStoreAdapter) List() ([]rtmodule.TrustKey, error) {
	raw, err := a.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]rtmodule.TrustKey, len(raw))
	for i, k := range raw {
		out[i] = toUsecaseTrustKey(k)
	}
	return out, nil
}

func (a keyStoreAdapter) Add(keyID string, pub ed25519.PublicKey) (rtmodule.TrustKey, error) {
	k, err := a.store.Add(keyID, pub)
	return toUsecaseTrustKey(k), err
}

func (a keyStoreAdapter) Remove(keyID string) error {
	return a.store.Remove(keyID)
}

type verifierAdapter struct {
	loader runtimemodule.Loader
}

func (a verifierAdapter) ParseSignature(sigRaw []byte) (string, []byte, error) {
	sig, err := runtimemodule.DecodeSignature(sigRaw)
	return sig.KeyID, sig.Bytes, err
}

func (a verifierAdapter) ParsePublicKey(keyRaw []byte) (ed25519.PublicKey, domain.Fingerprint, error) {
	pub, err := runtimemodule.ParsePublicKey(keyRaw)
	if err != nil {
		return nil, domain.Fingerprint{}, err
	}
	fp, err := runtimemodule.KeyFingerprint(pub)
	return pub, fp, err
}

func (a verifierAdapter) VerifyWithKey(pub ed25519.PublicKey, manifestRaw, sig []byte) error {
	return runtimemodule.VerifyWithKey(pub, manifestRaw, sig)
}

func (a verifierAdapter) VerifyStaging(stageDir, engine string) (rtmodule.VerifiedModule, error) {
	loaded, err := a.loader.VerifyDir(stageDir, engine, true)
	if err != nil {
		return rtmodule.VerifiedModule{}, err
	}
	return rtmodule.VerifiedModule{
		Engine:   engine,
		Manifest: loaded.Manifest,
		KeyID:    loaded.KeyID,
		Signed:   loaded.Signed,
		Warning:  loaded.Warning,
	}, nil
}

type moduleStoreAdapter struct {
	store runtimemodule.ModuleStore
}

func (a moduleStoreAdapter) Lock(ctx context.Context) (func(), error) {
	return a.store.Lock(ctx)
}

func (a moduleStoreAdapter) Get(engine string) (rtmodule.VerifiedModule, error) {
	loaded, err := a.store.Get(engine)
	if err != nil {
		return rtmodule.VerifiedModule{}, err
	}
	return rtmodule.VerifiedModule{
		Engine:   engine,
		Manifest: loaded.Manifest,
		KeyID:    loaded.KeyID,
		Signed:   loaded.Signed,
		Warning:  loaded.Warning,
	}, nil
}

func (a moduleStoreAdapter) List() []rtmodule.VerifiedModule {
	raw := a.store.List()
	out := make([]rtmodule.VerifiedModule, len(raw))
	for i, r := range raw {
		out[i] = rtmodule.VerifiedModule{
			Engine:   r.Engine,
			Manifest: r.Loaded.Manifest,
			KeyID:    r.Loaded.KeyID,
			Signed:   r.Loaded.Signed,
			Warning:  r.Loaded.Warning,
			Err:      r.Err,
		}
	}
	return out
}

func (a moduleStoreAdapter) Commit(stageDir, engine string, upgrade bool) error {
	return a.store.Commit(stageDir, engine, upgrade)
}

func (a moduleStoreAdapter) Remove(engine string) error {
	return a.store.Remove(engine)
}

func (a moduleStoreAdapter) ReadSource(engine string) (rtmodule.ModuleSource, error) {
	src, err := a.store.ReadSource(engine)
	if err != nil {
		return rtmodule.ModuleSource{}, err
	}
	return rtmodule.ModuleSource{
		Repo:  src.Repo,
		Tag:   src.Tag,
		KeyID: src.KeyID,
	}, nil
}

type nodeRestarter struct {
	fn func(ctx context.Context) error
}

func (r nodeRestarter) Restart(ctx context.Context) error {
	if r.fn == nil {
		return nil
	}
	return r.fn(ctx)
}

// runtimeUsageChecker checks whether any base or branch uses engine.
// When the agent is running, it queries GET /v1/bases and GET /v1/branches over
// the local UNIX socket (covering cluster-wide Raft state). When the agent is
// stopped, a single node falls back to <DataDir>/state.json, while a clustered
// node refuses safely until the agent is started.
type runtimeUsageChecker struct {
	cfg config.Config
}

func (u runtimeUsageChecker) InUse(ctx context.Context, engine domain.Engine) ([]string, []string, error) {
	c := cli.NewSocketClient(u.cfg.Socket)
	var baseResp struct {
		Items []struct {
			Name    string `json:"name"`
			Runtime string `json:"runtime"`
		} `json:"items"`
	}
	err := c.Do(ctx, "GET", "/v1/bases", nil, &baseResp)
	if err == nil {
		var branchResp struct {
			Items []struct {
				Name        string `json:"name"`
				Runtime     string `json:"runtime"`
				BaseRuntime string `json:"base_runtime"`
			} `json:"items"`
		}
		if err := c.Do(ctx, "GET", "/v1/branches", nil, &branchResp); err != nil {
			return nil, nil, err
		}
		var bases, branches []string
		for _, b := range baseResp.Items {
			if runtimeEngine(b.Runtime) == engine {
				bases = append(bases, b.Name)
			}
		}
		for _, br := range branchResp.Items {
			if runtimeEngine(br.Runtime) == engine || runtimeEngine(br.BaseRuntime) == engine {
				branches = append(branches, br.Name)
			}
		}
		return bases, branches, nil
	}
	var apiErr *cli.APIError
	if errors.As(err, &apiErr) {
		return nil, nil, err
	}
	cc, ccErr := LoadClusterConfig(u.cfg)
	if ccErr != nil {
		return nil, nil, ccErr
	}
	if cc != nil {
		return nil, nil, fmt.Errorf("%w: agent is not running on a clustered node; start db-branch.service before removing runtime module %q so cluster-wide usage can be verified", domain.ErrConflict, engine)
	}
	st := &filestore.Store{Path: filepath.Join(u.cfg.DataDir, "state.json")}
	rawBases, err := st.ListBases(ctx)
	if err != nil {
		return nil, nil, err
	}
	rawBranches, err := st.ListBranches(ctx)
	if err != nil {
		return nil, nil, err
	}
	var bases, branches []string
	for _, b := range rawBases {
		if b.Runtime.Engine == engine {
			bases = append(bases, b.Name.String())
		}
	}
	for _, br := range rawBranches {
		if br.Runtime.Engine == engine {
			branches = append(branches, br.Name.String())
		}
	}
	return bases, branches, nil
}

func runtimeEngine(raw string) domain.Engine {
	eng, _, _ := strings.Cut(raw, "@")
	return domain.Engine(eng)
}
