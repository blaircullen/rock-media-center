package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/tristenlammi/arrmada/internal/auth"
)

// The create and update API accept user_agent, the list returns it, and blank means not set.
func TestIndexerAPIUserAgent(t *testing.T) {
	a, svc := indexerAPI(t)
	ctx := context.Background()

	w := callIndexer(t, a.handleCreateIndexer, http.MethodPost, "/api/v1/indexers", "",
		`{"name":"Geek","kind":"torznab","url":"http://geek/api","user_agent":"  Arrmada/1.0  "}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		ID        int64  `json:"id"`
		UserAgent string `json:"user_agent"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.UserAgent != "Arrmada/1.0" {
		t.Fatalf("create response user_agent = %q", created.UserAgent)
	}
	if got, _ := svc.Get(ctx, created.ID); got.UserAgent != "Arrmada/1.0" {
		t.Fatalf("stored user agent = %q", got.UserAgent)
	}
	_, list := listIndexersAs(t, a, auth.RoleManager)
	if len(list) != 1 || list[0]["user_agent"] != "Arrmada/1.0" {
		t.Fatalf("list = %v", list)
	}

	id := strconv.FormatInt(created.ID, 10)
	if w := callIndexer(t, a.handleUpdateIndexer, http.MethodPut, "/api/v1/indexers/"+id, id,
		`{"name":"Geek","kind":"torznab","url":"http://geek/api","user_agent":"Other/2","enabled":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("update = %d: %s", w.Code, w.Body.String())
	}
	if got, _ := svc.Get(ctx, created.ID); got.UserAgent != "Other/2" {
		t.Fatalf("after update = %q", got.UserAgent)
	}
	if w := callIndexer(t, a.handleUpdateIndexer, http.MethodPut, "/api/v1/indexers/"+id, id,
		`{"name":"Geek","kind":"torznab","url":"http://geek/api","enabled":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("update = %d", w.Code)
	}
	if got, _ := svc.Get(ctx, created.ID); got.UserAgent != "" {
		t.Fatalf("blank should clear, got %q", got.UserAgent)
	}
	_, list = listIndexersAs(t, a, auth.RoleManager)
	if _, present := list[0]["user_agent"]; present {
		t.Fatalf("unset user_agent should be omitted: %v", list[0])
	}
}
