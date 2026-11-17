package store

import "testing"

func TestSplitWords(t *testing.T) {
	cases := map[string]string{
		"ProcessPayment":            "process payment",
		"payment_service":           "payment service",
		"HTTPClient":                "http client",
		"parseJSONBody":             "parse json body",
		"payments.PaymentService":   "payments payment service",
		"internal/store.Store.Open": "internal store store open",
		"OAuth2Token":               "o auth 2 token",
	}
	for in, want := range cases {
		if got := SplitWords(in); got != want {
			t.Errorf("SplitWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFTSQueryQuotesEveryTerm(t *testing.T) {
	cases := map[string]string{
		"rate limiter":     `"rate"* "limiter"*`,
		`"payment*)(`:      `"payment"*`,
		"   ":              "",
		"NEAR(a OR b)":     `"NEAR"* "a"* "OR"* "b"*`,
		"snake_case_names": `"snake_case_names"*`,
	}
	for in, want := range cases {
		if got := ftsQuery(in); got != want {
			t.Errorf("ftsQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
