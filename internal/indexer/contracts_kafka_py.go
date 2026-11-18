package indexer

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Kafka, Python. Consumers provide a topic, producers use it.
//
//	kafka-python     KafkaProducer().send(topic, ...); KafkaConsumer(topic, ..., group_id=...)
//	aiokafka         AIOKafkaProducer().send(topic, ...) and send_and_wait; AIOKafkaConsumer(topic, ...)
//	confluent-kafka  Producer(conf).produce(topic, ...); Consumer(conf).subscribe([topics])
//
// Any consumer's subscribe([topics]) consumes too. Clients are followed
// through local variables and self attributes.

// pyKafkaClasses maps the client classes, by full name, to "P" (producer)
// or "C" (consumer).
var pyKafkaClasses = map[string]string{
	"kafka.KafkaProducer": "P", "kafka.KafkaConsumer": "C",
	"aiokafka.AIOKafkaProducer": "P", "aiokafka.AIOKafkaConsumer": "C",
	"confluent_kafka.Producer": "P", "confluent_kafka.Consumer": "C",
	"confluent_kafka.SerializingProducer": "P", "confluent_kafka.DeserializingConsumer": "C",
}

// kafkaClient reports what a call creates: "P" for a producer, "C" and its
// group for a consumer, "" for anything else.
func (pf *pyFile) kafkaClient(call *sitter.Node) (kind, group string) {
	if call == nil || call.Type() != "call" {
		return "", ""
	}
	fn := text(field(call, "function"), pf.src)
	first, rest, _ := strings.Cut(fn, ".")
	full := pf.imports[first]
	if rest != "" {
		full += "." + rest
	}
	// kafka.consumer.group.KafkaConsumer and the like: by package and class.
	pkg, _, _ := strings.Cut(full, ".")
	kind = pyKafkaClasses[pkg+"."+full[strings.LastIndex(full, ".")+1:]]
	if kind == "C" {
		for _, a := range namedChildren(field(call, "arguments")) {
			if a.Type() == "keyword_argument" && text(field(a, "name"), pf.src) == "group_id" {
				if g := pf.template(field(a, "value")); !strings.Contains(g, "*") {
					group = g
				}
			}
		}
	}
	return kind, group
}

// kafkaClassClients collects the self attributes a class sets to a Kafka
// client in any of its methods.
func (pf *pyFile) kafkaClassClients(body *sitter.Node) map[string]string {
	clients := map[string]string{}
	walk(body, func(n *sitter.Node) bool {
		if n.Type() == "class_definition" && n != body {
			return false
		}
		if n.Type() != "assignment" {
			return true
		}
		left := field(n, "left")
		if left == nil || left.Type() != "attribute" || text(field(left, "object"), pf.src) != "self" {
			return true
		}
		if kind, group := pf.kafkaClient(field(n, "right")); kind != "" {
			clients[text(field(left, "attribute"), pf.src)] = kind + group
		}
		return true
	})
	return clients
}

// kafkaCall recognizes a consumer created with its topics, a producer
// sending to a topic, or a consumer subscribing. locals maps local
// variables to what kafkaClient found (kind, then group).
func (pf *pyFile) kafkaCall(n *sitter.Node, caller string, locals map[string]string) []store.Contract {
	l := line(n)
	topic := func(role string, a *sitter.Node, group string) (store.Contract, bool) {
		t := pf.template(a)
		if t == "" || t == "*" {
			return store.Contract{}, false
		}
		return kafkaContract(role, t, caller, group, l), true
	}
	var positional []*sitter.Node
	kw := map[string]*sitter.Node{}
	for _, a := range namedChildren(field(n, "arguments")) {
		if a.Type() == "keyword_argument" {
			kw[text(field(a, "name"), pf.src)] = field(a, "value")
		} else {
			positional = append(positional, a)
		}
	}
	var out []store.Contract
	// KafkaConsumer("orders", "payments", group_id="billing")
	if kind, group := pf.kafkaClient(n); kind == "C" {
		for _, a := range positional {
			if c, ok := topic(store.RoleProvides, a, group); ok {
				out = append(out, c)
			}
		}
		return out
	}
	fn := field(n, "function")
	if fn == nil || fn.Type() != "attribute" {
		return nil
	}
	obj, method := field(fn, "object"), text(field(fn, "attribute"), pf.src)
	client := ""
	switch {
	case obj == nil:
	case obj.Type() == "identifier":
		client = locals[text(obj, pf.src)]
	case obj.Type() == "attribute" && text(field(obj, "object"), pf.src) == "self":
		client = pf.kafka[text(field(obj, "attribute"), pf.src)]
	}
	if client == "" {
		return nil
	}
	kind, group := client[:1], client[1:]
	arg := func(name string) *sitter.Node {
		if v := kw[name]; v != nil {
			return v
		}
		if len(positional) > 0 {
			return positional[0]
		}
		return nil
	}
	switch {
	case kind == "P" && (method == "send" || method == "send_and_wait" || method == "produce"):
		if a := arg("topic"); a != nil {
			if c, ok := topic(store.RoleUses, a, ""); ok {
				out = append(out, c)
			}
		}
	case kind == "C" && method == "subscribe":
		if list := arg("topics"); list != nil && (list.Type() == "list" || list.Type() == "tuple" || list.Type() == "set") {
			for _, a := range namedChildren(list) {
				if c, ok := topic(store.RoleProvides, a, group); ok {
					out = append(out, c)
				}
			}
		}
	}
	return out
}
