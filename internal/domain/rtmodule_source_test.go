package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestParseGitHubRef_owner_repoと任意tagを解釈し不正な参照を拒否する(t *testing.T) {
	okCases := []struct {
		raw  string
		want domain.GitHubRef
	}{
		{
			raw:  "github.com/acme/rt-ch",
			want: domain.GitHubRef{Owner: "acme", Repo: "rt-ch"},
		},
		{
			raw:  "github.com/acme/rt-ch@v1.2.3",
			want: domain.GitHubRef{Owner: "acme", Repo: "rt-ch", Tag: "v1.2.3"},
		},
		{
			raw:  "github.com/Acme_01/db-branch.rt-ch@v0.2.0-rc.1+build.4",
			want: domain.GitHubRef{Owner: "Acme_01", Repo: "db-branch.rt-ch", Tag: "v0.2.0-rc.1+build.4"},
		},
	}
	for _, tc := range okCases {
		got, err := domain.ParseGitHubRef(tc.raw)
		if err != nil {
			t.Errorf("ParseGitHubRef(%q) = err %v, want %+v", tc.raw, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseGitHubRef(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
		if got.String() != tc.raw {
			t.Errorf("GitHubRef.String() = %q, want %q", got.String(), tc.raw)
		}
	}

	badCases := map[string]string{
		"tagにパーセントエンコード": "github.com/acme/rt-ch@v1%2Fx",
		"tagの先頭が記号":      "github.com/acme/rt-ch@-v1",
		"空文字":            "",
		"httpスキーム":       "http://github.com/acme/rt-ch",
		"httpsスキーム":      "https://github.com/acme/rt-ch",
		"別ホスト":           "gitlab.com/acme/rt-ch",
		"repo欠落":         "github.com/acme",
		"余計なパス":          "github.com/acme/rt-ch/extra",
		"ownerが空":        "github.com//rt-ch",
		"repoが空":         "github.com/acme/",
		"ownerに..":       "github.com/../rt-ch",
		"ownerに途中..":     "github.com/a..b/rt-ch",
		"ownerが.":        "github.com/./rt-ch",
		"repoに..":        "github.com/acme/..",
		"repoに途中..":      "github.com/acme/rt..ch",
		"repoが.":         "github.com/acme/.",
		"ownerに許可外文字":    "github.com/ac$me/rt-ch",
		"repoに許可外文字":     "github.com/acme/rt?ch",
		"空tag":           "github.com/acme/rt-ch@",
		"空白を含む":          "github.com/acme/rt-ch @v1.0.0",
		"tagに空白":         "github.com/acme/rt-ch@v1.0.0 beta",
		"tagにタブ":         "github.com/acme/rt-ch@v1.0.0\tbeta",
		"tagに制御文字":       "github.com/acme/rt-ch@v1.0.0\n",
		"tagにNUL":        "github.com/acme/rt-ch@v1.0.0\x00",
		"tagにDEL":        "github.com/acme/rt-ch@v1.0.0\x7f",
		"tagに?":          "github.com/acme/rt-ch@v1.0.0?foo=1",
		"tagに#":          "github.com/acme/rt-ch@v1.0.0#frag",
		"tagに/":          "github.com/acme/rt-ch@release/v1.0.0",
		"tagにバックスラッシュ":   `github.com/acme/rt-ch@v1.0.0\x`,
		"tagに..":         "github.com/acme/rt-ch@../v1.0.0",
		"tagが.":          "github.com/acme/rt-ch@.",
		"先頭空白":           " github.com/acme/rt-ch",
		"末尾空白":           "github.com/acme/rt-ch ",
	}
	for name, raw := range badCases {
		if got, err := domain.ParseGitHubRef(raw); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("%s (%q): got %+v, err = %v, want ErrInvalidRef", name, raw, got, err)
		}
	}
}

func TestParseModuleAssetName_契約どおりのtar_gz名からengineとversionとarchを取り出し違反名を拒否する(t *testing.T) {
	okCases := []struct {
		name string
		want domain.ModuleAsset
	}{
		{
			name: "db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz",
			want: domain.ModuleAsset{Engine: "clickhouse", Version: "0.2.0", Arch: "amd64"},
		},
		{
			name: "db-branch-rt-valkey-7_1.0.0-rc.1_linux_arm64.tar.gz",
			want: domain.ModuleAsset{Engine: "valkey-7", Version: "1.0.0-rc.1", Arch: "arm64"},
		},
		{
			name: "db-branch-rt-a123456789012345678_0.10.2+build.1_linux_amd64.tar.gz",
			want: domain.ModuleAsset{Engine: "a123456789012345678", Version: "0.10.2+build.1", Arch: "amd64"},
		},
	}
	for _, tc := range okCases {
		got, err := domain.ParseModuleAssetName(tc.name)
		if err != nil {
			t.Errorf("ParseModuleAssetName(%q) = err %v, want %+v", tc.name, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseModuleAssetName(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	badCases := map[string]string{
		"deb拡張子":       "db-branch-rt-clickhouse_0.2.0_linux_amd64.deb",
		"zip拡張子":       "db-branch-rt-clickhouse_0.2.0_linux_amd64.zip",
		"拡張子なし":        "db-branch-rt-clickhouse_0.2.0_linux_amd64",
		"組み込みmysql":    "db-branch-rt-mysql_0.2.0_linux_amd64.tar.gz",
		"組み込みpostgres": "db-branch-rt-postgres_0.2.0_linux_amd64.tar.gz",
		"19文字超エンジン":    "db-branch-rt-abcdefghijklmnopqrst_0.2.0_linux_amd64.tar.gz",
		"大文字エンジン":      "db-branch-rt-ClickHouse_0.2.0_linux_amd64.tar.gz",
		"空エンジン":        "db-branch-rt-_0.2.0_linux_amd64.tar.gz",
		"不正バージョン":      "db-branch-rt-clickhouse_not-a-ver_linux_amd64.tar.gz",
		"v付きバージョン":     "db-branch-rt-clickhouse_v0.2.0_linux_amd64.tar.gz",
		"パッチ欠落バージョン":   "db-branch-rt-clickhouse_0.2_linux_amd64.tar.gz",
		"先頭ゼロバージョン":    "db-branch-rt-clickhouse_01.2.0_linux_amd64.tar.gz",
		"非linuxのOS":    "db-branch-rt-clickhouse_0.2.0_darwin_amd64.tar.gz",
		"未対応arch":      "db-branch-rt-clickhouse_0.2.0_linux_386.tar.gz",
		"空arch":        "db-branch-rt-clickhouse_0.2.0_linux_.tar.gz",
		"プレフィックス違い":    "db-branch-clickhouse_0.2.0_linux_amd64.tar.gz",
		"パス区切りを含む":     "../db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz",
		"要素過多":         "db-branch-rt-clickhouse_0.2.0_extra_linux_amd64.tar.gz",
		"要素不足":         "db-branch-rt-clickhouse_0.2.0_amd64.tar.gz",
		"空文字":          "",
	}
	for name, raw := range badCases {
		if got, err := domain.ParseModuleAssetName(raw); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("%s (%q): got %+v, err = %v, want ErrInvalidRef", name, raw, got, err)
		}
	}
}

func TestParseFingerprint_sha256プレフィックスと64桁小文字hexのみ受理し定数時間で照合する(t *testing.T) {
	validHex := strings.Repeat("0123456789abcdef", 4)
	validStr := "sha256:" + validHex
	otherStr := "sha256:" + strings.Repeat("fedcba9876543210", 4)

	fp1, err := domain.ParseFingerprint(validStr)
	if err != nil {
		t.Fatalf("ParseFingerprint(%q) = %v", validStr, err)
	}
	if fp1.String() != validStr {
		t.Errorf("String() = %q, want %q", fp1.String(), validStr)
	}
	fp1Dup, err := domain.ParseFingerprint(validStr)
	if err != nil {
		t.Fatalf("ParseFingerprint dup = %v", err)
	}
	fp2, err := domain.ParseFingerprint(otherStr)
	if err != nil {
		t.Fatalf("ParseFingerprint other = %v", err)
	}
	if !fp1.Equal(fp1Dup) {
		t.Error("identical fingerprints must be equal")
	}
	if fp1.Equal(fp2) {
		t.Error("different fingerprints must not be equal")
	}
	var zero domain.Fingerprint
	if zero.Equal(zero) || fp1.Equal(zero) || zero.Equal(fp1) {
		t.Error("zero fingerprint must never match")
	}

	badCases := map[string]string{
		"空文字":       "",
		"プレフィックス欠落": validHex,
		"別プレフィックス":  "sha512:" + validHex,
		"大文字hex":    "sha256:" + strings.ToUpper(validHex),
		"長さ不足":      "sha256:" + validHex[:63],
		"長さ超過":      "sha256:" + validHex + "0",
		"非hex文字":    "sha256:" + strings.Repeat("g", 64),
		"空白混入":      "sha256: " + validHex,
		"末尾改行":      validStr + "\n",
	}
	for name, raw := range badCases {
		if got, err := domain.ParseFingerprint(raw); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("%s (%q): got %q, err = %v, want ErrInvalidRef", name, raw, got.String(), err)
		}
	}

	// NewFingerprint from 32-byte public key
	pub := make([]byte, 32)
	for i := range pub {
		pub[i] = byte(i)
	}
	wantSum := sha256.Sum256(pub)
	wantFP := "sha256:" + hex.EncodeToString(wantSum[:])
	fromPub, err := domain.NewFingerprint(pub)
	if err != nil {
		t.Fatalf("NewFingerprint(32 bytes) = %v", err)
	}
	if fromPub.String() != wantFP {
		t.Errorf("NewFingerprint() = %q, want %q", fromPub.String(), wantFP)
	}
	parsedWant, err := domain.ParseFingerprint(wantFP)
	if err != nil || !fromPub.Equal(parsedWant) {
		t.Errorf("NewFingerprint does not equal ParseFingerprint(%q): err=%v", wantFP, err)
	}
	for _, badPub := range [][]byte{nil, {}, make([]byte, 31), make([]byte, 33)} {
		if _, err := domain.NewFingerprint(badPub); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("NewFingerprint(len=%d): err = %v, want ErrInvalidRef", len(badPub), err)
		}
	}
}

func TestCompareModuleVersion_アップグレード判定で新しい版のみ許可し同一版と古い版を拒否する(t *testing.T) {
	// SemVer 2.0.0 §11 precedence order (strictly ascending)
	ordered := []string{
		"0.1.0",
		"0.2.0-rc.1",
		"0.2.0",
		"0.2.1",
		"0.10.0",
		"1.0.0-1",
		"1.0.0-2",
		"1.0.0-10",
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"2.0.0",
	}
	for i := 0; i < len(ordered); i++ {
		for j := 0; j < len(ordered); j++ {
			got, err := domain.CompareModuleVersion(ordered[i], ordered[j])
			if err != nil {
				t.Fatalf("CompareModuleVersion(%q, %q) = err %v", ordered[i], ordered[j], err)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("CompareModuleVersion(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}

	// Leading v and build metadata ignored
	equalPairs := [][2]string{
		{"v0.2.0", "0.2.0"},
		{"0.2.0+build.1", "0.2.0+build.2"},
		{"v0.2.0-rc.1+sha.aaa", "0.2.0-rc.1+sha.bbb"},
	}
	for _, pair := range equalPairs {
		got, err := domain.CompareModuleVersion(pair[0], pair[1])
		if err != nil || got != 0 {
			t.Errorf("CompareModuleVersion(%q, %q) = %d, %v, want 0, nil", pair[0], pair[1], got, err)
		}
	}

	// Invalid versions
	invalid := []string{
		"", "0.2", "1.2.", "0.2.0.1", "vv0.2.0", "V0.2.0", "01.2.0", "0.02.0",
		"0.2.0-01", "0.2.0-", "0.2.0+", "0.2.0+bad!", "0.2.0-a..b",
		"99999999999999999999999.0.0", "0.2.0-bad_id",
	}
	for _, v := range invalid {
		if _, err := domain.CompareModuleVersion(v, "0.2.0"); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("CompareModuleVersion(%q, 0.2.0): err = %v, want ErrInvalidRef", v, err)
		}
		if _, err := domain.CompareModuleVersion("0.2.0", v); !errors.Is(err, domain.ErrInvalidRef) {
			t.Errorf("CompareModuleVersion(0.2.0, %q): err = %v, want ErrInvalidRef", v, err)
		}
	}

	// Upgrade checks: strictly newer only
	allowUpgrades := [][2]string{
		{"0.1.0", "0.2.0"},
		{"0.2.0-rc.1", "0.2.0"},
		{"v0.2.0", "v0.10.0"},
	}
	for _, pair := range allowUpgrades {
		if err := domain.CheckModuleUpgrade(pair[0], pair[1]); err != nil {
			t.Errorf("CheckModuleUpgrade(%q -> %q) = %v, want nil", pair[0], pair[1], err)
		}
	}

	refuseUpgrades := [][2]string{
		{"0.2.0", "0.2.0"},
		{"0.2.0+a", "0.2.0+b"},
		{"0.2.0", "0.1.9"},
		{"0.2.0", "0.2.0-rc.1"},
		{"v0.10.0", "v0.2.0"},
	}
	for _, pair := range refuseUpgrades {
		if err := domain.CheckModuleUpgrade(pair[0], pair[1]); !errors.Is(err, domain.ErrDowngradeRefused) {
			t.Errorf("CheckModuleUpgrade(%q -> %q) = %v, want ErrDowngradeRefused", pair[0], pair[1], err)
		}
	}
	if err := domain.CheckModuleUpgrade("bad", "0.2.0"); !errors.Is(err, domain.ErrInvalidRef) {
		t.Errorf("CheckModuleUpgrade(bad -> 0.2.0) = %v, want ErrInvalidRef", err)
	}
}
