//go:build dev

package app

import _ "embed"

// The development licence root (test licences only).
//
//go:embed dev_license_root.pem
var devLicenseRoot []byte

func init() { licenseRootPEM = devLicenseRoot }
