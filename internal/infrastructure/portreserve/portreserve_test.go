package portreserve

import (
	"os"
	"path/filepath"
	"testing"
)

func use(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "reserved")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := Path
	Path = p
	t.Cleanup(func() { Path = old })
	return p
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReserve(t *testing.T) {
	cases := []struct {
		name, have, want string
		changed          bool
	}{
		{"empty", "\n", "40000-40999\n", true},
		{"keeps others", "8080,9000-9010\n", "8080,9000-9010,40000-40999\n", true},
		{"joins overlap", "40500-41500\n", "40000-41500\n", true},
		{"joins adjacent", "39990-39999,41000\n", "39990-41000\n", true},
		{"already covered", "30000-50000\n", "30000-50000\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := use(t, c.have)
			changed, err := Reserve(40000, 40999)
			if err != nil {
				t.Fatal(err)
			}
			if changed != c.changed || read(t, p) != c.want {
				t.Fatalf("changed=%v content=%q, want %v %q", changed, read(t, p), c.changed, c.want)
			}
		})
	}
}

func TestReserveSinglePortsAndUnsortedInput(t *testing.T) {
	p := use(t, "50000, 7000-7001\n")
	if _, err := Reserve(7002, 7002); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "7000-7002,50000\n" {
		t.Fatalf("got %q", got)
	}
}

func TestReserveErrors(t *testing.T) {
	use(t, "")
	for _, r := range [][2]int{{0, 10}, {20, 10}, {1, 70000}} {
		if _, err := Reserve(r[0], r[1]); err == nil {
			t.Fatalf("%v: no error", r)
		}
	}
	use(t, "abc\n")
	if _, err := Reserve(1, 2); err == nil {
		t.Fatal("bad content: no error")
	}
	use(t, "5-x\n")
	if _, err := Reserve(1, 2); err == nil {
		t.Fatal("bad range: no error")
	}
	Path = filepath.Join(t.TempDir(), "missing")
	if _, err := Reserve(1, 2); err == nil {
		t.Fatal("missing file: no error")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "ro")
	if err := os.WriteFile(p, []byte(""), 0o444); err != nil {
		t.Fatal(err)
	}
	Path = p
	if os.Getuid() != 0 {
		if _, err := Reserve(1, 2); err == nil {
			t.Fatal("read-only file: no error")
		}
	}
}
