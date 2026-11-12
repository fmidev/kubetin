package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/kubernetes/scheme"
)

// An evaluation error matters only when no authorizer decided: an allow
// or an explicit deny from another authorizer still answers the review.
func TestCanISeparatesUndecidedFromDenied(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  authv1.SubjectAccessReviewStatus
		allowed bool
		err     bool
	}{
		{"allowed", authv1.SubjectAccessReviewStatus{Allowed: true}, true, false},
		{"allowed despite an erroring authorizer", authv1.SubjectAccessReviewStatus{Allowed: true, EvaluationError: "webhook down"}, true, false},
		{"no opinion", authv1.SubjectAccessReviewStatus{Reason: "no RBAC policy matched"}, false, false},
		{"denied despite an erroring authorizer", authv1.SubjectAccessReviewStatus{Denied: true, EvaluationError: "webhook down"}, false, false},
		{"undecided", authv1.SubjectAccessReviewStatus{EvaluationError: "webhook down"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var review authv1.SelfSubjectAccessReview
				raw, _ := io.ReadAll(r.Body)
				if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(raw, nil, &review); err != nil {
					t.Error(err)
					return
				}
				review.Status = tc.status
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(review)
			}))
			defer srv.Close()
			sup, _ := newProbeFixture(t, srv, "")
			res := sup.CanI(context.Background(), "slow", "list", "fleet.cattle.io", "bundles", "")
			if res.Allowed != tc.allowed || (res.Err != "") != tc.err {
				t.Fatalf("got %+v", res)
			}
		})
	}
}
