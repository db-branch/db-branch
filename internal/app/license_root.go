package app

import (
	"crypto/ecdsa"

	"github.com/db-branch/db-branch/pkg/ext/license"
)

// licenseRootPEM is the public key licence keys are checked against, set by
// a build-tagged file: license_root_prod.go (production key, the default)
// or license_root_dev.go (-tags dev, test licences only).
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
