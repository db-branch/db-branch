package licenseclient

import (
	"net/http"
	"testing"
)

func TestClient_既定HTTPクライアントは再利用されタイムアウトと専用Transportを持つ(t *testing.T) {
	c := &Client{}
	h1 := c.client()
	h2 := c.client()
	if h1 != h2 {
		t.Error("expected default http.Client to be reused across calls")
	}
	if h1 == http.DefaultClient || h1.Timeout != Timeout {
		t.Errorf("unexpected default client: %+v", h1)
	}
	tr, ok := h1.Transport.(*http.Transport)
	if !ok || tr == nil || tr == http.DefaultTransport || tr.ResponseHeaderTimeout <= 0 {
		t.Errorf("expected dedicated http.Transport with ResponseHeaderTimeout, got %#v", h1.Transport)
	}
}
