package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/db-branch/db-branch/internal/infrastructure/clientcfg"
)

// ProfileInfo stores persisted connection and TLS settings for a remote profile.
type ProfileInfo struct {
	URL          string `json:"url"`
	CACert       string `json:"ca_cert,omitempty"`
	TLSSHA256    string `json:"tls_sha256,omitempty"`
	TokenStorage string `json:"token_storage,omitempty"`
}

// ClientConfigData holds all saved remote profiles and the active selection.
type ClientConfigData struct {
	CurrentProfile string                 `json:"current_profile"`
	Profiles       map[string]ProfileInfo `json:"profiles"`
}

// ClientConfigStore loads and saves client.toml.
type ClientConfigStore interface {
	Path() string
	Load() (ClientConfigData, error)
	Save(ClientConfigData) error
}

// SecretStore persists Bearer tokens per profile.
type SecretStore interface {
	Save(profile, token string) (storage string, warn string, err error)
	Load(profile string) (token string, err error)
	Delete(profile string) error
}

// CertSummary describes a remote server's leaf TLS certificate.
type CertSummary struct {
	Subject     string
	Issuer      string
	NotBefore   time.Time
	NotAfter    time.Time
	SANs        []string
	SHA256      string
	SANMismatch bool
}

// UntrustedCertError signals a certificate rejected by system CAs during login probing.
type UntrustedCertError struct {
	Cert CertSummary
	Err  error
}

func (e *UntrustedCertError) Error() string {
	return fmt.Sprintf("untrusted TLS certificate (sha256:%s): %v", e.Cert.SHA256, e.Err)
}

func (e *UntrustedCertError) Unwrap() error { return e.Err }

// RemoteOptions configures an HTTPS API client.
type RemoteOptions struct {
	URL          string
	Token        string
	CACert       string
	PinnedSHA256 string
}

type fileClientConfigStore struct{ path string }

// FileClientConfigStore returns a ClientConfigStore backed by client.toml at path.
func FileClientConfigStore(path string) ClientConfigStore {
	return fileClientConfigStore{path: path}
}

func (s fileClientConfigStore) Path() string { return s.path }

func (s fileClientConfigStore) Load() (ClientConfigData, error) {
	cfg, err := clientcfg.Load(s.path)
	if err != nil {
		return ClientConfigData{}, err
	}
	out := ClientConfigData{
		CurrentProfile: cfg.CurrentProfile,
		Profiles:       make(map[string]ProfileInfo, len(cfg.Profiles)),
	}
	for k, p := range cfg.Profiles {
		out.Profiles[k] = ProfileInfo{
			URL:          p.URL,
			CACert:       p.CACert,
			TLSSHA256:    p.TLSSHA256,
			TokenStorage: p.TokenStorage,
		}
	}
	return out, nil
}

func (s fileClientConfigStore) Save(data ClientConfigData) error {
	cfg := clientcfg.Config{
		CurrentProfile: data.CurrentProfile,
		Profiles:       make(map[string]clientcfg.Profile, len(data.Profiles)),
	}
	for k, p := range data.Profiles {
		cfg.Profiles[k] = clientcfg.Profile{
			URL:          p.URL,
			CACert:       p.CACert,
			TLSSHA256:    p.TLSSHA256,
			TokenStorage: p.TokenStorage,
		}
	}
	return clientcfg.Save(s.path, cfg)
}

// apiErrorSource adapts *remotehttp.APIError to *cli.APIError without coupling packages.
type apiErrorSource interface {
	error
	APIErrorParts() (status int, code, message string)
}

type apiErrorClient struct{ next Client }

func (c apiErrorClient) Do(ctx context.Context, method, path string, in, out any) error {
	err := c.next.Do(ctx, method, path, in, out)
	if err == nil {
		return nil
	}
	var src apiErrorSource
	if errors.As(err, &src) {
		st, code, msg := src.APIErrorParts()
		return &APIError{Status: st, Code: code, Message: msg}
	}
	return err
}

func (a *App) getenv(key string) string {
	if a.Getenv != nil {
		return a.Getenv(key)
	}
	return os.Getenv(key)
}

func (a *App) clientStores() (ClientConfigStore, SecretStore, error) {
	if a.NewClientStores == nil {
		return nil, nil, errors.New("remote client stores are not configured")
	}
	cfgPath := firstNonEmpty(a.clientConfigFlag, a.getenv("DB_BRANCH_CONFIG"))
	return a.NewClientStores(cfgPath)
}

func (a *App) useRemoteTransport() bool {
	return a.ClientOnly ||
		a.serverFlag != "" ||
		a.profileFlag != "" ||
		a.getenv("DB_BRANCH_URL") != "" ||
		a.getenv("DB_BRANCH_PROFILE") != ""
}

func (a *App) resolveRemoteOptions() (RemoteOptions, error) {
	explicitProfile := firstNonEmpty(a.profileFlag, a.getenv("DB_BRANCH_PROFILE"))
	explicitServer := firstNonEmpty(a.serverFlag, a.getenv("DB_BRANCH_URL"))
	explicitCA := firstNonEmpty(a.caFlag, a.getenv("DB_BRANCH_CA"))
	explicitPin := a.getenv("DB_BRANCH_TLS_SHA256")
	explicitToken := a.getenv("DB_BRANCH_TOKEN")

	if explicitCA != "" && explicitPin != "" {
		return RemoteOptions{}, errors.New("specify either --ca / DB_BRANCH_CA or DB_BRANCH_TLS_SHA256, not both")
	}

	// When --server / DB_BRANCH_URL is set without --profile, never fall back to
	// current_profile's token or TLS pin to prevent sending credentials to the wrong host.
	if explicitServer != "" && explicitProfile == "" {
		if explicitToken == "" {
			return RemoteOptions{}, errors.New("DB_BRANCH_TOKEN is required when --server or DB_BRANCH_URL is set without --profile (or run `db-branch login`)")
		}
		return RemoteOptions{
			URL:          strings.TrimSpace(explicitServer),
			Token:        explicitToken,
			CACert:       explicitCA,
			PinnedSHA256: explicitPin,
		}, nil
	}

	cfgStore, secStore, err := a.clientStores()
	if err != nil {
		return RemoteOptions{}, err
	}
	cfg, err := cfgStore.Load()
	if err != nil {
		return RemoteOptions{}, err
	}
	profName := firstNonEmpty(explicitProfile, cfg.CurrentProfile)
	if profName == "" {
		return RemoteOptions{}, errors.New("not logged in; run `db-branch login --server <url>` or set DB_BRANCH_URL and DB_BRANCH_TOKEN")
	}
	if err := clientcfg.ValidateProfileName(profName); err != nil {
		return RemoteOptions{}, err
	}
	prof, ok := cfg.Profiles[profName]
	if !ok {
		return RemoteOptions{}, fmt.Errorf("profile %q not found; run `db-branch login --profile %s --server <url>`", profName, profName)
	}
	serverURL := firstNonEmpty(explicitServer, prof.URL)
	ca := explicitCA
	pin := explicitPin
	switch {
	case ca != "":
		pin = ""
	case pin != "":
		ca = ""
	default:
		ca = prof.CACert
		if ca == "" {
			pin = prof.TLSSHA256
		}
	}
	tok := explicitToken
	if tok == "" {
		tok, err = secStore.Load(profName)
		if err != nil {
			return RemoteOptions{}, fmt.Errorf("load token for profile %q: %w (re-run `db-branch login --profile %s`)", profName, err, profName)
		}
	}
	return RemoteOptions{
		URL:          serverURL,
		Token:        tok,
		CACert:       ca,
		PinnedSHA256: pin,
	}, nil
}

func (a *App) buildRemoteClient(opts RemoteOptions) (Client, error) {
	if a.RemoteClient == nil {
		return nil, errors.New("HTTPS client is not configured")
	}
	c, err := a.RemoteClient(opts)
	if err != nil {
		return nil, err
	}
	return apiErrorClient{next: c}, nil
}

func (a *App) remoteClient() (Client, error) {
	opts, err := a.resolveRemoteOptions()
	if err != nil {
		return nil, err
	}
	return a.buildRemoteClient(opts)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
