package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gett/internal/errs"
)

func TestInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	strict, err := NewClient(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Get(srv.URL); errs.Code(err) != 60 {
		t.Fatalf("checked certificate: code %d (%v), want 60", errs.Code(err), err)
	}

	loose, err := NewClient(Config{Insecure: true, Retries: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := loose.Get(srv.URL)
	if err != nil {
		t.Fatalf("insecure: %v", err)
	}
	resp.Body.Close()
}
