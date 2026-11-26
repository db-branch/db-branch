// Package clientapp wires remote HTTPS transport, config, and keychain into cli.App.
package clientapp

import (
	"context"
	"errors"

	"github.com/db-branch/db-branch/internal/infrastructure/clientcfg"
	"github.com/db-branch/db-branch/internal/infrastructure/keychain"
	"github.com/db-branch/db-branch/internal/infrastructure/remotehttp"
	"github.com/db-branch/db-branch/internal/interface/cli"
)

// Configure populates remote client hooks on a without importing server-side packages.
func Configure(a *cli.App) {
	a.NewClientStores = func(configPath string) (cli.ClientConfigStore, cli.SecretStore, error) {
		p, err := clientcfg.ResolvePath(configPath)
		if err != nil {
			return nil, nil, err
		}
		return cli.FileClientConfigStore(p), keychain.New(clientcfg.CredentialsPath(p)), nil
	}
	a.RemoteClient = func(opts cli.RemoteOptions) (cli.Client, error) {
		return remotehttp.NewClient(remotehttp.Options{
			URL:          opts.URL,
			Token:        opts.Token,
			CACert:       opts.CACert,
			PinnedSHA256: opts.PinnedSHA256,
		})
	}
	a.ProbeTLS = func(ctx context.Context, rawURL, caCert, expectedSHA256 string) (cli.CertSummary, error) {
		info, err := remotehttp.ProbeTLS(ctx, rawURL, caCert, expectedSHA256)
		var uce *remotehttp.UntrustedCertError
		if errors.As(err, &uce) {
			sum := toSummary(uce.Cert)
			return sum, &cli.UntrustedCertError{Cert: sum, Err: uce.Err}
		}
		if err != nil {
			return cli.CertSummary{}, err
		}
		return toSummary(info), nil
	}
}

func toSummary(c remotehttp.CertInfo) cli.CertSummary {
	return cli.CertSummary{
		Subject:     c.Subject,
		Issuer:      c.Issuer,
		NotBefore:   c.NotBefore,
		NotAfter:    c.NotAfter,
		SANs:        c.SANs,
		SHA256:      c.SHA256,
		SANMismatch: c.SANMismatch,
	}
}
