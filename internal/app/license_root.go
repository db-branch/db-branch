package app

import (
	"crypto/ecdsa"

	"github.com/db-branch/db-branch/pkg/ext/license"
)

// licenseRootPEM is the public key licence keys are checked against, set by
// a build-tagged file (license_root_dev.go with -tags dev). Release builds
// get the production key once it exists; until then they cannot verify
// keys in the console (modules still check them themselves).
var licenseRootPEM []byte

func licenseRoot() *ecdsa.PublicKey {
	if len(licenseRootPEM) == 0 {
		return nil
	}
	k, err := license.ParsePublicKey(licenseRootPEM)
	if err != nil {
		return nil
	}
	return k
}
