//go:build !dev

package app

import _ "embed"

// The production licence root public key (the same key the modules embed).
// Every build without -tags dev, the release build, carries it; it never
// falls back to the development key, which only a -tags dev build has.
// Only the public key lives in the repository.
//
//go:embed prod_license_root.pem
var prodLicenseRoot []byte

func init() { licenseRootPEM = prodLicenseRoot }
