package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/db-branch/db-branch/internal/domain"
)

func TestNameとRuntimeRefはJSONで文字列として往復する(t *testing.T) {
	type doc struct {
		Name    domain.Name       `json:"name"`
		Runtime domain.RuntimeRef `json:"runtime"`
	}
	n, _ := domain.ParseName("feature-x")
	in := doc{Name: n, Runtime: domain.RuntimeRef{Engine: domain.MySQL, Version: "8.4.13"}}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"feature-x","runtime":"mysql@8.4.13"}` {
		t.Errorf("json = %s", b)
	}
	var out doc
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip = %+v", out)
	}
}

func Test不正な名前やランタイムはJSONから読み込めない(t *testing.T) {
	var n domain.Name
	if err := json.Unmarshal([]byte(`"Bad Name"`), &n); err == nil {
		t.Error("Name: want error")
	}
	var r domain.RuntimeRef
	if err := json.Unmarshal([]byte(`"mysql@latest"`), &r); err == nil {
		t.Error("RuntimeRef: want error")
	}
}

func Test未設定の名前やランタイムは空文字列として往復する(t *testing.T) {
	type doc struct {
		Name    domain.Name       `json:"name"`
		Runtime domain.RuntimeRef `json:"runtime"`
	}
	b, _ := json.Marshal(doc{})
	if string(b) != `{"name":"","runtime":""}` {
		t.Errorf("json = %s", b)
	}
	var out doc
	if err := json.Unmarshal(b, &out); err != nil || out != (doc{}) {
		t.Errorf("round trip = %+v, %v", out, err)
	}
}
