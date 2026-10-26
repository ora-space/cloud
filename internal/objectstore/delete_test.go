package objectstore

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeleteSignsThePrivateKeyAndTreatsMissingObjectsAsDone(t *testing.T) {
	for name, status := range map[string]int{"deleted": http.StatusNoContent, "already gone": http.StatusNotFound} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/revisions/scheduled-runs/tenant/run/0" || r.URL.Query().Get("X-Amz-Signature") == "" {
					t.Error("delete did not bind the private key, DELETE signature and path")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			if err := Delete(t.Context(), readConfig(server.URL), "scheduled-runs/tenant/run/0"); err != nil {
				t.Fatalf("delete = %v, want success", err)
			}
		})
	}
}

func TestDeleteReportsUncomfirmedAndRedirectedDeletes(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	if err := Delete(t.Context(), readConfig(failing.URL), "scheduled-runs/tenant/run/0"); !errors.Is(err, ErrDeleteUnavailable) {
		t.Fatalf("delete = %v, want the bounded unavailable classification", err)
	}

	// A redirect must not be followed: the signed capability names one endpoint and one key.
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.invalid/other", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	if err := Delete(t.Context(), readConfig(redirecting.URL), "scheduled-runs/tenant/run/0"); !errors.Is(err, ErrDeleteUnavailable) {
		t.Fatalf("redirected delete = %v, want the bounded unavailable classification", err)
	}
}
