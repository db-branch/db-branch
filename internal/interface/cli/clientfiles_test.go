package cli

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strings"
	"testing"
)

var uuidV5Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDeterministicUUID_標準のUUIDv5を決定的に返す(t *testing.T) {
	a := deterministicUUID("datagrip:alice:192.0.2.10:40001:alice")
	b := deterministicUUID("datagrip:alice:192.0.2.10:40001:alice")
	c := deterministicUUID("datagrip:bob:192.0.2.10:40001:bob")
	if a != b {
		t.Fatalf("deterministicUUID not deterministic: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("distinct seeds collided: %q", a)
	}
	if !uuidV5Pattern.MatchString(a) {
		t.Fatalf("not a valid UUID v5: %q", a)
	}
	const want = "070efc39-c71a-54ec-b5c7-5c66660f4b2e"
	if a != want {
		t.Fatalf("uuid5 = %q, want %q", a, want)
	}
}

func TestGenerateClientFile_制御文字を含む入力でも有効なXMLを生成する(t *testing.T) {
	conn := branchConnection{
		Engine:   "mysql",
		Host:     "192.0.2.10",
		Port:     40001,
		User:     "u\x00s\x07er<&>",
		Password: "pw",
		Database: "app\x1bdb",
		Branch:   "br\x0b\x0canch\"'",
	}
	for _, format := range []string{"datagrip", "navicat", "workbench"} {
		p, err := generateClientFile(conn, format, false)
		if err != nil {
			t.Fatalf("%s failed: %v", format, err)
		}
		if strings.ContainsAny(string(p.data), "\x00\x07\x0b\x0c\x1b") {
			t.Fatalf("%s output contains raw XML 1.0 forbidden control characters: %q", format, p.data)
		}
		var doc any
		if err := xml.Unmarshal(p.data, &doc); err != nil {
			t.Fatalf("%s produced invalid XML: %v\n%s", format, err, p.data)
		}
	}

	p, err := generateClientFile(conn, "dbeaver", false)
	if err != nil {
		t.Fatalf("dbeaver failed: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(p.data), int64(len(p.data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".xml") && !strings.HasSuffix(f.Name, "/.project") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := xml.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s produced invalid XML: %v\n%s", f.Name, err, b)
		}
	}
}
