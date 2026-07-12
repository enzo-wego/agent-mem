package handlers

import (
	"reflect"
	"testing"
)

func TestExtractIdentifiers(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "real partner raise keeps payment ref and request id",
			text: `Seems juspay has a limitation of the orders to be accessed on API.
We have some payments like pxx6xgkdtl need to be refunded but created more than 1 year ago.
 request_id: b2106f73-beed-477f-b82a-0e2b0abe6ba0,
 order_id: pxx6xgkdtl`,
			want: []string{"b2106f73-beed-477f-b82a-0e2b0abe6ba0", "pxx6xgkdtl"},
		},
		{
			name: "english words with the ref shape need a digit",
			text: "prevention protection of the process",
			want: nil,
		},
		{
			name: "charset excludes a p s so ordinary words never match",
			text: "processing payments dispatched superseded",
			want: nil,
		},
		{
			name: "jira keys and PR refs normalize",
			text: "see PAY-2111 and github.com/wego/payments/pull/1230 plus wego/payments#1230 again",
			want: []string{"PAY-2111", "wego/payments#1230"},
		},
		{
			name: "known payment refs from prod all match",
			text: "p960x3vfvh p09y9zd7h4 pyz99j7hoc p9696jn5v7 p6zz03ow83",
			want: []string{"p09y9zd7h4", "p6zz03ow83", "p960x3vfvh", "p9696jn5v7", "pyz99j7hoc"},
		},
		{
			name: "empty input",
			text: "",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractIdentifiers(tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractIdentifiers() = %v, want %v", got, tt.want)
			}
		})
	}
}
