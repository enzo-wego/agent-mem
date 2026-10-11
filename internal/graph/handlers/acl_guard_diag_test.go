package handlers

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fixedScopeBuilder struct {
	scopes []string
	err    error
}

func (b fixedScopeBuilder) For(context.Context, int) ([]string, error) { return b.scopes, b.err }

func TestAskerScopeSetDiagResolvedWithoutMembershipsIsFiltered(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	aclPerson(t, pool, 501, "zero@example.com")
	eeid, scopes, noFilter, err := askerScopeSetDiag(context.Background(), pool, fixedScopeBuilder{}, "zero@example.com")
	if err != nil || eeid != 501 || noFilter || !reflect.DeepEqual(scopes, map[string]bool{"public": true}) {
		t.Fatalf("eeid=%d scopes=%v noFilter=%t err=%v", eeid, scopes, noFilter, err)
	}
}

func TestAskerScopeSetDiagReportsMembershipFailureAndWrapperResultIsUnchanged(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	aclPerson(t, pool, 502, "failure@example.com")
	failure := errors.New("membership failed")
	builder := fixedScopeBuilder{err: failure}
	eeid, scopes, noFilter, err := askerScopeSetDiag(context.Background(), pool, builder, "failure@example.com")
	if !errors.Is(err, failure) || eeid != 502 || noFilter || !reflect.DeepEqual(scopes, map[string]bool{"public": true}) {
		t.Fatalf("diag eeid=%d scopes=%v noFilter=%t err=%v", eeid, scopes, noFilter, err)
	}
	oldEEID, oldScopes, oldNoFilter := askerScopeSet(context.Background(), pool, builder, "failure@example.com")
	if oldEEID != eeid || oldNoFilter != noFilter || !reflect.DeepEqual(oldScopes, scopes) {
		t.Fatalf("wrapper=(%d,%v,%t) diag=(%d,%v,%t)", oldEEID, oldScopes, oldNoFilter, eeid, scopes, noFilter)
	}
}
