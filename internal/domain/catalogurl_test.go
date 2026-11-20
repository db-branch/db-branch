package domain_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestIsPublicIP_公開アドレスだけを真にする(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "2001:4860:4860::8888", "172.15.0.1", "172.32.0.1", "100.63.255.255", "100.128.0.1"}
	for _, s := range public {
		if !domain.IsPublicIP(net.ParseIP(s)) {
			t.Errorf("%s must be public", s)
		}
	}
	private := []string{
		"127.0.0.1", "127.255.255.254", "::1", "10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fc00::1", "fd12:3456::1", "0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "ff02::1",
		"::127.0.0.1", "::169.254.169.254", "::10.0.0.1", "::ffff:0:169.254.169.254", "fec0::1", "fe80::1%eth0", "2001:2::1", "2001:10::1",
		"3fff::1", "4000::1", "8000::1", "e000::1", "1::1",
		"100.64.0.1", "100.127.255.255", "192.0.0.1", "192.0.2.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1",
		"240.0.0.1", "255.255.255.255", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "64:ff9b::7f00:1", "2001:db8::1",
	}
	for _, s := range private {
		if domain.IsPublicIP(net.ParseIP(s)) {
			t.Errorf("%s must not be public", s)
		}
	}
	if domain.IsPublicIP(nil) {
		t.Error("nil is not public")
	}
}

func fixedResolver(ips ...string) domain.Resolver {
	return func(context.Context, string) ([]net.IP, error) {
		var out []net.IP
		for _, s := range ips {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func TestValidateCatalogURL_httpsで公開アドレスに解決されるURLだけを受理する(t *testing.T) {
	ctx := context.Background()
	ok := []string{
		"https://downloads.example.com/clickhouse/25.3.1/clickhouse-amd64.tgz",
		"https://downloads.example.com:8443/x",
		"https://93.184.216.34/x",
		"https://[2606:4700:4700::1111]/x",
		"https://DOWNLOADS.example.com./x",
	}
	for _, u := range ok {
		if err := domain.ValidateCatalogURL(ctx, u, fixedResolver("93.184.216.34")); err != nil {
			t.Errorf("%s = %v", u, err)
		}
	}
}

func TestValidateCatalogURL_危険なURLを拒否する(t *testing.T) {
	ctx := context.Background()
	pub := fixedResolver("93.184.216.34")
	bad := map[string]string{
		"http":            "http://downloads.example.com/x",
		"ftp":             "ftp://downloads.example.com/x",
		"file":            "file:///etc/passwd",
		"スキームなし":          "downloads.example.com/x",
		"空":               "",
		"ホストなし":           "https:///x",
		"ユーザー情報":          "https://user:pw@downloads.example.com/x",
		"ユーザー名だけ":         "https://user@downloads.example.com/x",
		"ループバックのIPリテラル":   "https://127.0.0.1/x",
		"メタデータサービス":       "https://169.254.169.254/latest/meta-data/",
		"プライベートのIPリテラル":   "https://10.1.2.3/x",
		"IPv6ループバック":      "https://[::1]/x",
		"IPv4射影IPv6":      "https://[::ffff:127.0.0.1]/x",
		"localhost":       "https://localhost/x",
		"サブドメインlocalhost": "https://app.localhost/x",
		"ポート0":            "https://downloads.example.com:0/x",
		"ポートが範囲外":         "https://downloads.example.com:99999/x",
		"制御文字":            "https://downloads.example.com/x\r\nHost: evil",
		"空白":              "https://downloads.example.com/a b",
		"フラグメントではなくバックスラッシュのホスト": `https://evil.com\@127.0.0.1/x`,
	}
	for name, u := range bad {
		if err := domain.ValidateCatalogURL(ctx, u, pub); !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("%s (%q): err = %v, want ErrUnsafeURL", name, u, err)
		}
	}
}

func TestValidateCatalogURL_名前の解決結果が一つでも非公開なら拒否する(t *testing.T) {
	ctx := context.Background()
	for name, r := range map[string]domain.Resolver{
		"ループバックに解決":      fixedResolver("127.0.0.1"),
		"DNSリバインディング風":   fixedResolver("93.184.216.34", "169.254.169.254"),
		"プライベートだけ":       fixedResolver("10.0.0.5"),
		"何も返さない":         fixedResolver(),
		"解決に失敗":          func(context.Context, string) ([]net.IP, error) { return nil, errors.New("no such host") },
		"10進表記のIPに解決される": fixedResolver("127.0.0.1"),
	} {
		if err := domain.ValidateCatalogURL(ctx, "https://downloads.example.com/x", r); !errors.Is(err, domain.ErrUnsafeURL) {
			t.Errorf("%s: err = %v, want ErrUnsafeURL", name, err)
		}
	}
	if err := domain.ValidateCatalogURL(ctx, "https://2130706433/x", fixedResolver("127.0.0.1")); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("a decimal host that resolves to loopback = %v", err)
	}
}

func TestIsPublicIP_長さが4でも16でもないアドレスは公開ではない(t *testing.T) {
	for _, ip := range []net.IP{{}, {1}, {8, 8, 8}, make(net.IP, 5), make(net.IP, 17)} {
		if domain.IsPublicIP(ip) {
			t.Errorf("%v (%d bytes) was taken as public", ip, len(ip))
		}
	}
}

func TestValidateCatalogURL_リゾルバがなければ名前は解決できないとして拒否しpanicしない(t *testing.T) {
	if err := domain.ValidateCatalogURL(context.Background(), "https://downloads.example.com/x", nil); !errors.Is(err, domain.ErrUnsafeURL) {
		t.Errorf("err = %v, want ErrUnsafeURL", err)
	}
	if err := domain.ValidateCatalogURL(context.Background(), "https://93.184.216.34/x", nil); err != nil {
		t.Errorf("an address literal needs no resolver: %v", err)
	}
}
