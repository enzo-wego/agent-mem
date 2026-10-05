package handlers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestIdentifiersForNode_OwnKey(t *testing.T) {
	for _, tt := range []struct {
		id, typ, body string
		want          []string
	}{
		{"jira:PAY-2333", "jira", "Fix refunds", []string{"PAY-2333"}},
		{"jira:pay-2333", "jira", "PAY-2333 PAY-2333", []string{"PAY-2333"}},
		{"jira:PAY-2333", "jira", "", []string{"PAY-2333"}},
		{"gh_pr:wego/payments#2357", "gh_pr", "Fix refunds", []string{"wego/payments#2357"}},
		{"gh_pr:wego/pay_ops#12", "gh_pr", "wego/pay_ops#12", []string{"wego/pay_ops#12"}},
		{"jira:xPAY-1y", "jira", "", nil},
		{"jira:PAY-1234567", "jira", "", nil},
		{"slack:C1:1", "slack", "PAY-2333", nil},
		{"slack_thread:C1:1", "slack_thread", "PAY-2333", nil},
	} {
		t.Run(tt.id+"/"+tt.body, func(t *testing.T) {
			got, err := identifiersForNode(context.Background(), Deps{}, tt.id, tt.typ, "slack:D1", "1", "2", tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("identifiers=%v, want %v", got, tt.want)
			}
		})
	}
	var refs []string
	for i := range 64 {
		refs = append(refs, fmt.Sprintf("p%09d", i))
	}
	got, err := identifiersForNode(context.Background(), Deps{}, "jira:PAY-2333", "jira", "", "", "", strings.Join(refs, " ")+" PAY-1 PAY-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxIdentifiersPerNode || !sort.StringsAreSorted(got) {
		t.Fatalf("bounded sorted IDs=%v", got)
	}
	if !containsIdentifier(got, "PAY-2333") {
		t.Fatalf("own key lost: %v", got)
	}
}

func containsIdentifier(ids []string, key string) bool {
	for _, id := range ids {
		if id == key {
			return true
		}
	}
	return false
}
