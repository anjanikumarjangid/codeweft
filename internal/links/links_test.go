package links

import (
	"strings"
	"testing"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"/orders/{id}":                        "/orders/*",
		"/orders/:id/items":                   "/orders/*/items",
		"/stock/<int:sku>/":                   "/stock/*",
		"http://orders:8080/api/orders/*?x=1": "/api/orders/*",
		"*/inventory/stock/*":                 "/inventory/stock/*",
		"https://host":                        "/",
		"*":                                   "",
		"/files/*rest":                        "/files/*",
		"/v1/items/order-*":                   "/v1/items/*",
	} {
		if got := store.NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func end(role, method, name, symbol string, exact bool) End {
	return End{ContractRow: store.ContractRow{Contract: store.Contract{Kind: store.ContractHTTP, Role: role,
		Method: method, Name: name, Symbol: symbol, Exact: exact}}}
}

func TestMatch(t *testing.T) {
	res := Match([]End{
		end(store.RoleProvides, "GET", "/orders/*", "getOrder", true),
		end(store.RoleProvides, "GET", "/orders/recent", "recent", true),
		end(store.RoleProvides, "POST", "/orders", "place", true),
		end(store.RoleProvides, "GET", "/health", "health", true),
		// The most specific route wins: /orders/recent, not /orders/*.
		end(store.RoleUses, "GET", "/orders/recent", "a", true),
		end(store.RoleUses, "GET", "/orders/*", "b", false),
		// A method that conflicts does not link; an unknown one may.
		end(store.RoleUses, "DELETE", "/orders", "c", true),
		end(store.RoleUses, "", "/orders", "d", true),
		end(store.RoleUses, "GET", "/payments/*", "e", false),
	})
	got := map[string][]string{}
	for _, l := range res.Links {
		got[l.From.Symbol] = append(got[l.From.Symbol], l.To.Symbol+"/"+l.Confidence)
	}
	want := map[string][]string{
		"a": {"recent/exact"},
		"b": {"getOrder/pattern", "recent/pattern"}, // equally specific: both
		"d": {"place/exact"},
	}
	for k, w := range want {
		if len(got[k]) != len(w) {
			t.Errorf("links of %s = %v, want %v", k, got[k], w)
			continue
		}
		for i := range w {
			if got[k][i] != w[i] {
				t.Errorf("links of %s = %v, want %v", k, got[k], w)
			}
		}
	}
	if len(got["c"]) != 0 || len(got["e"]) != 0 {
		t.Errorf("c and e should not link: %v %v", got["c"], got["e"])
	}
	if len(res.Unmatched) != 2 || len(res.Unreferring) != 1 || res.Unreferring[0].Symbol != "health" {
		t.Errorf("unmatched uses %d (want 2), unused providers %+v (want health)", len(res.Unmatched), res.Unreferring)
	}
}

// TestTaskQueue: a workflow started on one task queue is not served by a
// worker polling another, even under the same name.
func TestTaskQueue(t *testing.T) {
	wf := func(role, sym, queue string) End {
		return End{ContractRow: store.ContractRow{Contract: store.Contract{Kind: store.ContractTemporalWorkflow, Role: role,
			Name: "Ship", Symbol: sym, Detail: queue, Exact: true}}}
	}
	res := Match([]End{
		wf(store.RoleProvides, "eu.Ship", "ship-eu"),
		wf(store.RoleProvides, "us.Ship", "ship-us"),
		wf(store.RoleUses, "startEU", "ship-eu"),
		wf(store.RoleUses, "startAny", ""),
	})
	var got []string
	for _, l := range res.Links {
		got = append(got, l.From.Symbol+" -> "+l.To.Symbol)
	}
	want := "startAny -> eu.Ship, startAny -> us.Ship, startEU -> eu.Ship"
	if strings.Join(got, ", ") != want {
		t.Errorf("got %s, want %s", strings.Join(got, ", "), want)
	}
}

func TestAMQPKeyMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, key string
		want         bool
	}{
		{"order.*", "order.created", true},
		{"order.*", "order.created.eu", false},
		{"order.#", "order.created.eu", true},
		{"order.#", "order", true},
		{"#.eu", "order.created.eu", true},
		{"*.created", "invoice.created", true},
		{"order.created", "order.created", true},
		{"order.created", "order.paid", false},
		{"", "anything", true}, // a fanout binding
		{"#", "", true},
	} {
		if got := amqpKeyMatch(c.pattern, c.key); got != c.want {
			t.Errorf("amqpKeyMatch(%q, %q) = %v, want %v", c.pattern, c.key, got, c.want)
		}
	}
}
