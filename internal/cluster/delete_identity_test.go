package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeleteUsesSelectedUIDPrecondition(t *testing.T) {
	var options metav1.DeleteOptions
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/namespaces/default/pods/database" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status:   "Failure", Reason: metav1.StatusReasonConflict, Code: http.StatusConflict,
			Message: "UID precondition failed: resource was replaced",
		})
	}))
	defer server.Close()
	sup, _ := newProbeFixture(t, server, "")
	ref := DescribeRef{Version: "v1", Resource: "pods", Kind: "Pod", Namespace: "default", Name: "database", UID: "selected-uid"}
	res := sup.Delete(context.Background(), "slow", ref)
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != ref.UID {
		t.Fatalf("delete lost selected UID: %+v", options)
	}
	if res.OK || res.Err == "" || res.Context != "slow" || res.Ref != ref {
		t.Fatalf("replacement conflict lost action identity: %+v", res)
	}
}
