package store

import "testing"

func TestExpandProperties(t *testing.T) {
	p := properties{
		"orders.topic":  {{value: "root.orders", dir: "."}, {value: "svc.orders", dir: "orders/src/main/resources"}},
		"billing.topic": {{value: "prod.billing", profile: "prod", dir: "."}, {value: "billing", dir: "."}},
		"KAFKA_PREFIX":  {{value: "acme", dir: "."}},
		"events.topic":  {{value: "${kafka.prefix}.events", dir: "."}},
		"base.url":      {{value: "http://inventory:8080/api", dir: "."}},
	}
	for _, c := range []struct{ in, from, want string }{
		{"${orders.topic}", "orders/src/main/java/com/acme", "svc.orders"}, // the module's own configuration
		{"${orders.topic}", "web", "root.orders"},
		{"${billing.topic}", "x", "billing"},      // the base file before a profile's
		{"${ORDERS_TOPIC}", "web", "root.orders"}, // an environment variable, bound to orders.topic
		{"${kafka.prefix}", "x", "acme"},          // a property, set as KAFKA_PREFIX
		{"${events.topic}", "x", "acme.events"},   // nested
		{"${missing:fallback}", "x", "fallback"},  // the default
		{"${missing}", "x", "${missing}"},         // left as written
		{"${base.url}/stock/*", "x", "http://inventory:8080/api/stock/*"},
	} {
		if got := p.expand(c.in, c.from); got != c.want {
			t.Errorf("expand(%q, %q) = %q, want %q", c.in, c.from, got, c.want)
		}
	}

	http := ContractRow{Contract: Contract{Kind: ContractHTTP, Role: RoleUses, Name: "/stock/*", Detail: "${base.url}/stock/*"}, File: "web/client.go"}
	p.resolve(&http)
	if http.Name != "/api/stock/*" || http.Exact {
		t.Errorf("http: %+v", http)
	}
	topic := ContractRow{Contract: Contract{Kind: ContractKafka, Role: RoleUses, Name: "${orders.topic}"}, File: "web/x.java"}
	p.resolve(&topic)
	if topic.Name != "root.orders" || !topic.Exact {
		t.Errorf("kafka: %+v", topic)
	}
	unknown := ContractRow{Contract: Contract{Kind: ContractKafka, Role: RoleUses, Name: "${nope}"}, File: "x"}
	p.resolve(&unknown)
	if unknown.Name != "${nope}" || unknown.Exact {
		t.Errorf("unresolved: %+v", unknown)
	}
}
