package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
)

// This exercises the actual fixture call site: rebuilding the complete contract for every
// fixture made the full race suite exceed its existing timeout. Sharing is safe only if each
// response remains strictly validated and concurrent reads leave the contract unchanged.
func TestIntegrationResponseContractSharedAndImmutable(t *testing.T) {
	first, second := setup(t), setup(t)
	a, ok := first.client.HTTP.Transport.(*validatingTransport)
	if !ok {
		t.Fatal("first fixture has no contract validator")
	}
	b, ok := second.client.HTTP.Transport.(*validatingTransport)
	if !ok {
		t.Fatal("second fixture has no contract validator")
	}
	if a.routes != b.routes {
		t.Fatal("fixtures rebuilt the same immutable OpenAPI contract")
	}
	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/v1/me/git-identity", nil)
	route, _, err := a.routes.FindRoute(req)
	must(t, err)
	before, err := json.Marshal(route.Spec)
	must(t, err)
	const workers = 16
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { results <- validateSharedContract(a.routes) })
	}
	wg.Wait()
	close(results)
	for err := range results {
		must(t, err)
	}
	after, err := json.Marshal(route.Spec)
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("response validation mutated the shared contract")
	}
}

func validateSharedContract(routes routers.Router) error {
	for _, payload := range []struct {
		body  string
		valid bool
	}{
		{`{"name":"Ada","email":"ada@example.invalid","isDefault":true,"version":0}`, true},
		{`{"name":"Ada","email":"ada@example.invalid","isDefault":true,"version":0,"extra":true}`, false},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/api/v1/me/git-identity", nil)
		route, params, err := routes.FindRoute(req)
		if err != nil {
			return err
		}
		input := &openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}, Status: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Options: &openapi3filter.Options{IncludeResponseStatus: true}}
		input.SetBodyBytes([]byte(payload.body))
		err = openapi3filter.ValidateResponse(context.Background(), input)
		if (err == nil) != payload.valid {
			return fmt.Errorf("shared response validation accepted=%t, want %t", err == nil, payload.valid)
		}
	}
	return nil
}
