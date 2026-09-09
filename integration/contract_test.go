package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/contract"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

type validatingTransport struct {
	base     http.RoundTripper
	cloudURL string
	routes   routers.Router
}

func (v *validatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, e := v.base.RoundTrip(req)
	if e != nil || !strings.HasPrefix(req.URL.String(), v.cloudURL) {
		return res, e
	}
	body, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil {
		return nil, e
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	route, params, e := v.routes.FindRoute(req)
	if e != nil {
		return nil, e
	}
	input := &openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}, Status: res.StatusCode, Header: res.Header, Options: &openapi3filter.Options{IncludeResponseStatus: true}}
	input.SetBodyBytes(body)
	if e = openapi3filter.ValidateResponse(req.Context(), input); e != nil {
		res.Body.Close()
		return nil, fmt.Errorf("OpenAPI response mismatch %s %s: %w", req.Method, req.URL.Path, e)
	}
	return res, nil
}

func validateHTTP(t *testing.T, f *fixture) {
	t.Helper()
	b, e := json.Marshal(contract.Document())
	must(t, e)
	doc, e := openapi3.NewLoader().LoadFromData(b)
	must(t, e)
	doc.Servers = nil
	routes, e := legacy.NewRouter(doc)
	must(t, e)
	f.client.HTTP.Transport = &validatingTransport{base: http.DefaultTransport, cloudURL: f.cloud.URL, routes: routes}
}

func TestRestartRecreatesCloudSubstrateAndController(t *testing.T) {
	f := setup(t)
	validateHTTP(t, f)
	created := f.create("durable-create")
	wid := created.O("workspace").S("id")
	f.substrate.SetFault("sandbox_ensure", "lose_response")
	if e := f.controller.Drain(context.Background()); e == nil {
		t.Fatal("expected lost sandbox response")
	}
	// Rebuild every service object from PostgreSQL and the same filesystem, discard controller memory.
	f.cloud.Close()
	f.external.Close()
	must(t, f.store.Pool.Close())
	auth, e := core.NewAuthenticator("ora-cloud", f.client.Credentials.Trust)
	must(t, e)
	reopened := &core.Store{Pool: stdlib.OpenDB(*f.pgConfig)}
	t.Cleanup(func() { reopened.Pool.Close() })
	must(t, reopened.CheckSchema(context.Background()))
	f.store = reopened
	cloud := httptest.NewServer(router.New(reopened, auth, zap.NewNop()))
	t.Cleanup(cloud.Close)
	substrate, e := simulator.NewSubstrate(f.substrate.Root, f.substrate.Repositories)
	must(t, e)
	external := httptest.NewServer(substrate)
	t.Cleanup(external.Close)
	client := &simulator.Client{URL: cloud.URL, Credentials: f.client.Credentials, HTTP: &http.Client{}, Subject: "controller-after-restart"}
	f.client = client
	f.cloud = cloud
	validateHTTP(t, f)
	_, e = f.store.Pool.Exec("UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second'")
	must(t, e)
	_, e = f.store.Pool.Exec("UPDATE operations SET retry_at=clock_timestamp()-interval '1 second' WHERE id=$1", created.O("operation").S("id"))
	must(t, e)
	controller := &simulator.Controller{Client: client, SubstrateURL: external.URL}
	must(t, controller.Acquire(context.Background()))
	must(t, controller.Drain(context.Background()))
	if f.ws(wid).S("observedState") != "ready" || f.scalar("SELECT count(*) FROM sandbox_instances WHERE workspace_id=$1", wid) != 1 || f.scalar("SELECT count(*) FROM external_effects WHERE workspace_id=$1 AND kind='worktree_ensure'", wid) != 1 {
		t.Fatal("restart depended on discarded service memory")
	}
	replay := f.create("durable-create")
	if replay.O("workspace").S("id") != wid {
		t.Fatal("cloud restart lost idempotency record")
	}
}

func TestSameHolderRestartsRunningOperationFromPersistedEffectIntent(t *testing.T) {
	f := setup(t)
	created := f.create("same-holder-restart")
	operationID := created.O("operation").S("id")
	claimed := f.internal("/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch}, 200).O("operation")
	planned := f.internal("/internal/v1/operations/"+operationID+"/effects", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version"), "kind": "storage_ensure"}, 200)
	request := planned.O("effect").O("request")
	if request.S("kind") != "storage_ensure" || request.S("projectId") != created.O("resource").S("id") {
		t.Fatal("effect intent was not persisted", request)
	}

	restarted := &simulator.Controller{Client: f.client, SubstrateURL: f.external.URL}
	must(t, restarted.Acquire(context.Background()))
	if restarted.Epoch != f.controller.Epoch {
		t.Fatal("same holder unexpectedly changed lease epoch")
	}
	must(t, restarted.Drain(context.Background()))
	if f.ws(created.O("workspace").S("id")).S("observedState") != "ready" {
		t.Fatal("same-holder restart did not recover running operation")
	}
	f.internal("/internal/v1/operations/"+operationID+"/advance", core.Object{"epoch": f.controller.Epoch, "version": planned.O("operation").N("version")}, 409)
}

func TestSchemaCheckRejectsUnknownMigration(t *testing.T) {
	f := setup(t)
	_, err := f.store.Pool.Exec("INSERT INTO schema_migrations(version,checksum) VALUES('9999_future.sql','future')")
	must(t, err)
	err = f.store.CheckSchema(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown to this binary") {
		t.Fatal("older binary accepted a newer schema", err)
	}
}
