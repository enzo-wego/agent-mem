package handlers

import "testing"

func TestIsAlertChannelName(t *testing.T) {
	for _, name := range []string{"payments-alerts", "payment-alert", "alerts-payments"} {
		if !isAlertChannelName(name) {
			t.Fatalf("%q should be classified as an alert channel", name)
		}
	}
	for _, name := range []string{"payments-dev", "alerting-design", "sales"} {
		if isAlertChannelName(name) {
			t.Fatalf("%q should not be classified as an alert channel", name)
		}
	}
}

func TestAlertFingerprintStripsVolatileValues(t *testing.T) {
	a := alertFingerprint("PaymentFailed order 123456 amount 50.00 at 2026-07-09T10:11:12Z")
	b := alertFingerprint("PaymentFailed order 999999 amount 70.00 at 2026-07-09T10:12:13Z")
	if a == "" {
		t.Fatal("fingerprint is empty")
	}
	if a != b {
		t.Fatalf("fingerprints differ for same template:\n%s\n%s", a, b)
	}
}
