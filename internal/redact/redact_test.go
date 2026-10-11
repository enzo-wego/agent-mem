package redact

import "testing"

// All data is synthetic. 4111111111111111 is the public Luhn-valid test PAN.
const (
	pan       = "4111111111111111"
	panSpaced = "4111 1111 1111 1111"
	panHyph   = "4111-1111-1111-1111"
	nonLuhn16 = "1234567890123456"
)

func TestMask(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		// emails
		{"email plain", "mail alice@example.com now", "mail [email] now"},
		{"email person id", "person:alice@example.com", "person:[email]"},
		{"email in url", "https://example.com/u?e=alice@example.com&x=1", "https://example.com/u?e=[email]&x=1"},
		{"email mailto", "mailto:alice@example.com", "mailto:[email]"},
		{"email partner id", "partner:alice@example.com", "partner:[email]"},
		{"email status id", "status:alice@example.com", "status:[email]"},
		{"email code_file id", "code_file:exports/alice@example.com.csv", "code_file:exports/[email]"},
		{"email partner prose", "see partner:alice@example.com for details", "see partner:[email] for details"},
		{"email url prose", "see https://example.com/?e=alice@example.com ok", "see https://example.com/?e=[email] ok"},

		// phones
		{"phone intl compact", "+6581234567", "[phone]"},
		{"phone intl spaced", "call +65 8123 4567 today", "call [phone] today"},
		{"phone intl hyphen", "+65-8123-4567", "[phone]"},
		{"phone intl dots", "+65.8123.4567", "[phone]"},
		{"phone intl parens", "+1 (415) 555-0100", "[phone]"},
		{"phone intl paren cc", "+(65) 8123 4567", "[phone]"},
		{"phone labelled local", "phone 81234567", "phone [phone]"},
		{"phone labelled leading zero", "mobile: 0812 3456 7890", "mobile: [phone]"},
		{"phone luhn plus", "+4111111111111", "[phone]"},
		{"phone whatsapp", "whatsapp 81234567.", "whatsapp [phone]."},

		// cards
		{"card plain", "pan " + pan + " end", "pan [card] end"},
		{"card unlabelled luhn", "x " + pan + " y", "x [card] y"},
		{"card spaced", panSpaced, "[card]"},
		{"card hyphenated", panHyph, "[card]"},
		{"card labelled non-luhn", "card " + nonLuhn16, "card [card]"},
		{"card labelled 12 digits", "visa 123456789012", "visa [card]"},
		{"card amex 15", "378282246310005", "[card]"},

		// unchanged
		{"F ref numeric", "F.4111111111111111", "F.4111111111111111"},
		{"F ref alnum", "ref F.abc123 ok", "ref F.abc123 ok"},
		{"F ref and pan", "F.4111111111111111 paid with " + pan, "F.4111111111111111 paid with [card]"},
		{"payment ref", "payment p12345678z failed", "payment p12345678z failed"},
		{"action ref", "action a123456789012bc ok", "action a123456789012bc ok"},
		{"booking ref numeric", "booking ref: 4111111111111111", "booking ref: 4111111111111111"},
		{"booking hash", "booking #4111111111111111", "booking #4111111111111111"},
		{"order equals", "order=987654321", "order=987654321"},
		{"slack ts", "ts 1700000000.123456", "ts 1700000000.123456"},
		{"permalink", "https://x.slack.com/archives/C012ABC/p1700000000123456", "https://x.slack.com/archives/C012ABC/p1700000000123456"},
		{"uuid", "id 123e4567-e89b-12d3-a456-426614174000", "id 123e4567-e89b-12d3-a456-426614174000"},
		{"eeid", "eeid 1234", "eeid 1234"},
		{"page id", "cf:1234567890123", "cf:1234567890123"},
		{"slack node id", "slack:C012ABC:1700000000.123456", "slack:C012ABC:1700000000.123456"},
		{"slack node id ts only", "slack:C012ABC:1700000000.123456 and 1700000000.123456", "slack:C012ABC:1700000000.123456 and 1700000000.123456"},
		{"url no email", "https://example.com/p/4111111111111111", "https://example.com/p/4111111111111111"},
		{"non-luhn 16 unlabelled", nonLuhn16, nonLuhn16},
		{"amount", "total USD 12,345.67 paid", "total USD 12,345.67 paid"},
		{"jira key", "jira:PAY-1234 fixed", "jira:PAY-1234 fixed"},
		{"empty", "", ""},
		{"no digits", "hello world", "hello world"},

		// boundaries
		{"too short card", "card 12345678901", "card 12345678901"},
		{"too long run", "card 41111111111111111111", "card 41111111111111111111"},
		{"glued letters left", "x" + pan, "x" + pan},
		{"glued letters right", pan + "x", pan + "x"},
		{"short phone plus", "+1234567", "+1234567"},
		{"two side by side", pan + " " + pan, "[card] [card]"},
		{"two phones", "+6581234567 / +6591234567", "[phone] / [phone]"},
		{"card then phone", pan + " +6581234567", "[card] [phone]"},
		{"number before pan", "100 " + pan, "100 [card]"},

		// r2 regressions, each alone
		{"card colon", "card:" + pan, "card:[card]"},
		{"phone colon", "phone:81234567", "phone:[phone]"},
		{"phone equals", "phone=81234567", "phone=[phone]"},
		{"tel colon plus", "tel:+6581234567", "tel:[phone]"},
		{"pan inline code", "`" + pan + "`", "`[card]`"},
		{"pan fenced", "```\n" + pan + "\n```", "```\n[card]\n```"},
		{"bold plus phone", "**+6581234567**", "**[phone]**"},
		{"cf pair", "cf:4111111111111111 and card:" + pan, "cf:4111111111111111 and card:[card]"},
		{"datadog pair", "datadog:monitor:4111111111111111 " + pan, "datadog:monitor:4111111111111111 [card]"},
		{"slack id pair", "slack:C012ABC:1700000000.123456 " + pan, "slack:C012ABC:1700000000.123456 [card]"},
		{"payment ref pair", "p12345678z " + pan, "p12345678z [card]"},

		// booking binding
		{"booking then card", "booking 7 card " + pan, "booking 7 card [card]"},
		{"booking value then phone", "booking ref: AB12345 phone +6581234567", "booking ref: AB12345 phone [phone]"},
		{"booking across newline", "booking ref: AB12345\nphone +6581234567", "booking ref: AB12345\nphone [phone]"},
		{"booking value then card", "order: 98765 pan " + pan, "order: 98765 pan [card]"},
		{"booking luhn value then card", "pnr: " + pan + " card " + pan, "pnr: " + pan + " card [card]"},
		{"bare ref is not booking label", "ref: " + pan, "ref: [card]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Mask(tc.in); got != tc.want {
				t.Errorf("Mask(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Sole canary in free text must be masked, with structural ids beside it kept.
func TestMaskCanaryBesideStructure(t *testing.T) {
	t.Parallel()
	structure := []string{
		"cf:4111111111111111", "datadog:monitor:4111111111111111",
		"slack:C012ABC:1700000000.123456", "p12345678z", "F.4111111111111111",
	}
	canaries := map[string]string{
		"card:4111111111111111": "card:[card]", "phone:81234567": "phone:[phone]",
		"phone=81234567": "phone=[phone]", "tel:+6581234567": "tel:[phone]",
		"`4111111111111111`": "`[card]`", "**+6581234567**": "**[phone]**",
		"alice@example.com": "[email]",
	}
	for _, s := range structure {
		for in, want := range canaries {
			if got := Mask(in + " " + s); got != want+" "+s {
				t.Errorf("Mask(%q beside %q) = %q", in, s, got)
			}
			if got := Mask(s + " " + in); got != s+" "+want {
				t.Errorf("Mask(%q after %q) = %q", in, s, got)
			}
		}
	}
}

func TestMaskIdempotent(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"+6581234567 " + pan + " alice@example.com", "booking: AB12345 phone 81234567"} {
		once := Mask(in)
		if twice := Mask(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

// shift maps ASCII digits to another Unicode decimal-digit block starting at zero.
func shift(s string, zero rune) string {
	out := []rune(s)
	for i, r := range out {
		if r >= '0' && r <= '9' {
			out[i] = zero + (r - '0')
		}
	}
	return string(out)
}

func TestMaskURLBoundariesAndUnicodeDigits(t *testing.T) {
	t.Parallel()
	const u = "https://x.example/a"
	tests := []struct{ name, in, want string }{
		{"slack link phone label", "<" + u + "|+6591234567>", "<" + u + "|[phone]>"},
		{"slack link card label", "<" + u + "|" + pan + ">", "<" + u + "|[card]>"},
		{"jira link card", "[" + u + "|" + panSpaced + "]", "[" + u + "|[card]]"},
		{"json no spaces", `{"u":"` + u + `","p":"+6591234567","c":"` + pan + `"}`, `{"u":"` + u + `","p":"[phone]","c":"[card]"}`},
		{"comma phone", u + ",+6591234567", u + ",[phone]"},
		{"url digits readable", "https://wa.me/6591234567", "https://wa.me/6591234567"},
		{"arabic-indic plus", "+٩٦٦٥٠١٢٣٤٥٦٧", "[phone]"},
		{"arabic-indic label", "phone: ٠٥٠١٢٣٤٥٦٧", "phone: [phone]"},
		{"ext arabic-indic plus", "+" + shift("966501234567", '۰'), "[phone]"},
		{"fullwidth card", shift(pan, '０'), "[card]"},
		{"fullwidth card spaced", shift(panSpaced, '０'), "[card]"},
		{"fullwidth non-luhn unlabelled", shift(nonLuhn16, '０'), shift(nonLuhn16, '０')},
		{"percent email", "user%40example.com", "[email]"},
		{"percent email upper hex", "x user%40Example.COM y", "x [email] y"},
		{"percent email in url", "https://e.example/?e=user%40example.com", "https://e.example/?e=[email]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Mask(tt.in); got != tt.want {
				t.Fatalf("Mask(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMaskURLAllowlistFixRound2(t *testing.T) {
	t.Parallel()
	const u = "https://jira.example/PAY-1"
	const ph = "+6591234567"
	tests := []struct{ name, in, want string }{
		{"md link then phone", "[text](" + u + ")" + ph, "[text](" + u + ")[phone]"},
		{"paren url then card", "(" + u + ")" + pan, "(" + u + ")[card]"},
		{"paren dot phone", u + ")." + ph, u + ").[phone]"},
		{"paren colon card", u + "):" + pan, u + "):[card]"},
		{"dot phone", u + "." + ph, u + ".[phone]"},
		{"dot card", u + "." + pan, u + ".[card]"},
		{"colon card", u + ":" + pan, u + ":[card]"},
		{"bang card", u + "!" + pan, u + "![card]"},
		{"bang phone", u + "!" + ph, u + "![phone]"},
		{"open paren phone", u + "(" + ph, u + "([phone]"},
		{"open paren card", u + "(" + pan, u + "([card]"},
		{"plus phone glued", u + ph, u + "[phone]"},
		{"nbsp phone", u + "\u00a0" + ph, u + "\u00a0[phone]"},
		{"nbsp card", u + "\u00a0" + pan, u + "\u00a0[card]"},
		{"ideographic space card", u + "\u3000" + pan, u + "\u3000[card]"},
		{"thin space phone", u + "\u2009" + ph, u + "\u2009[phone]"},
		{"zero width space card", u + "\u200b" + pan, u + "\u200b[card]"},
		{"arabic comma phone", u + "\u060c" + ph, u + "\u060c[phone]"},
		{"fullwidth comma card", u + "\uff0c" + pan, u + "\uff0c[card]"},
		{"url dot plus phone", "https://x.example/p." + ph, "https://x.example/p.[phone]"},
		{"nbsp separated phone", "+96650\u00a01234567", "[phone]"},
		{"narrow nbsp phone", "+966\u202f501\u202f234567", "[phone]"},
		{"ideographic grouped card", "4111\u30001111\u30001111\u30001111", "[card]"},
		{"wa.me kept", "https://wa.me/6591234567", "https://wa.me/6591234567"},
		{"permalink kept", "https://wego.slack.com/archives/C012ABCDEF/p1700000000123456", "https://wego.slack.com/archives/C012ABCDEF/p1700000000123456"},
		{"percent query kept", "https://e.example/?q=a%40b", "https://e.example/?q=a%40b"},
		{"path digits kept", "https://x.example/items/4111111111111111/view", "https://x.example/items/4111111111111111/view"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Mask(tt.in); got != tt.want {
				t.Fatalf("Mask(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMaskFixRound3(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"pct2B form field", "customer_phone=%2B966501234567&x=1", "customer_phone=[phone]&x=1"},
		{"pct2b lowercase", "msisdn=%2b966501234567", "msisdn=[phone]"},
		{"pct2B in url query", "https://x.example/?to=%2B6591234567", "https://x.example/?to=[phone]"},
		{"pct2B in json", `{"phone":"%2B6591234567"}`, `{"phone":"[phone]"}`},
		{"jira id nbsp plus phone", "jira:PAY-1\u00a0+6591234567", "jira:PAY-1\u00a0[phone]"},
		{"jira id paren plus phone", "(jira:PAY-1)+6591234567", "(jira:PAY-1)[phone]"},
		{"jira id arabic comma phone", "jira:PAY-1\u060c+6591234567", "jira:PAY-1\u060c[phone]"},
		{"person type plus phone", "person:+6591234567", "person:[phone]"},
		{"date plus kept", "https://wiki.example/display/X/Incident+2026-10-12+Payments", "https://wiki.example/display/X/Incident+2026-10-12+Payments"},
		{"date title kept", "Incident+2026-10-12+Payments", "Incident+2026-10-12+Payments"},
		{"spaced plus phone masked", "+65 9123 4567", "[phone]"},
		{"pct2B form plus seps", "phone=%2B65+9123+4567&x=1", "phone=[phone]&x=1"},
		{"pct2B form plus seps sa", "phone=%2B966+50+123+4567", "phone=[phone]"},
		{"pct2B form plus seps us", "phone=%2B1+202+555+0143", "phone=[phone]"},
		{"pct2B form in url", "https://x/?phone=%2B65+9123+4567", "https://x/?phone=[phone]"},
		{"pct2B pct20 seps", "curl -d 'phone=%2B65%209123%204567'", "curl -d 'phone=[phone]'"},
		{"pct2B pct2D seps", "to=%2B65%2D9123%2D4567", "to=[phone]"},
		{"double encoded residual", "x=%252B6591234567", "x=%252B6591234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Mask(tt.in); got != tt.want {
				t.Fatalf("Mask(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
