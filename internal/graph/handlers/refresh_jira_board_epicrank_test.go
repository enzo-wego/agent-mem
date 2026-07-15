package handlers

import "testing"

func TestParseBoardEpicPage(t *testing.T) {
	body := []byte(`{"maxResults":50,"startAt":0,"isLast":false,"values":[` +
		`{"id":1,"key":"PAY-100","name":"A"},` +
		`{"id":2,"key":"PAY-50","name":"B"},` +
		`{"id":3,"key":"","name":"skip-blank"}]}`)

	keys, isLast, err := parseBoardEpicPage(body)
	if err != nil {
		t.Fatalf("parseBoardEpicPage: %v", err)
	}
	if isLast {
		t.Fatalf("isLast = true, want false")
	}
	want := []string{"PAY-100", "PAY-50"} // board order preserved; blank key dropped
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}
