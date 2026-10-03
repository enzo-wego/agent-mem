package handlers

import "testing"

func TestSubjectTitleQuery(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  string
	}{
		{"PAY-1: Foo bar", "Foo bar"},
		{"[UX] Home Page Callout", "Home Page Callout"},
		{"Foo", ""},
		{"PAY-1", ""},
		{"IN GST - Staging end-to-end", "IN GST - Staging end-to-end"},
		{"one two three four five six seven eight nine ten eleven twelve thirteen", ""},
	} {
		t.Run(tc.title, func(t *testing.T) {
			if got := subjectTitleQuery(tc.title); got != tc.want {
				t.Fatalf("subjectTitleQuery(%q) = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}
