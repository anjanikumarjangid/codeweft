package store

import (
	"strings"
	"testing"
)

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

func TestFTSWordsDropSyntax(t *testing.T) {
	cases := map[string]string{
		"rate limiter":     "rate limiter",
		`"payment*)(`:      "payment",
		"   ":              "",
		"NEAR(a OR b)":     "NEAR a OR b",
		"snake_case_names": "snake_case_names",
		"x:* & !y":         "x y",
	}
	for in, want := range cases {
		if got := strings.Join(ftsWords(in), " "); got != want {
			t.Errorf("ftsWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRebindPlaceholders(t *testing.T) {
	cases := map[string]string{
		`SELECT ? , ?`:                       `SELECT $1 , $2`,
		`WHERE a = ?1 OR b LIKE ?2 LIMIT ?3`: `WHERE a = $1 OR b LIKE $2 LIMIT $3`,
		`WHERE x = '?' AND y = ?`:            `WHERE x = '?' AND y = $1`,
		`ESCAPE '\' AND z = ?`:               `ESCAPE '\' AND z = $1`,
		`no placeholders`:                    `no placeholders`,
	}
	for in, want := range cases {
		if got := postgresDialect.rebind(in); got != want {
			t.Errorf("rebind(%q) = %q, want %q", in, got, want)
		}
		if got := sqliteDialect.rebind(in); got != in {
			t.Errorf("sqlite rebind changed %q to %q", in, got)
		}
	}
}
