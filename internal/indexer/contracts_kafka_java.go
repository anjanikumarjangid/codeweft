package indexer

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Kafka, Java. Consumers provide a topic, producers use it.
//
//	Spring Kafka   consumer: @KafkaListener(topics = ..., groupId = ...) on a
//	                         method, or on a class with @KafkaHandler methods
//	               producer: kafkaTemplate.send(topic, ...), send(new ProducerRecord<>(topic, ...)),
//	                         sendDefault(...) to ${spring.kafka.template.default-topic}
//	kafka-clients  consumer: consumer.subscribe(List.of(topics...))
//	               producer: producer.send(new ProducerRecord<>(topic, ...))
//
// A topic written as a property placeholder ("${orders.topic}"), directly
// or through an @Value field, is kept as written: a producer and a
// consumer reading the same property are linked, as a pattern.

// kafkaTopics evaluates an annotation's topics element: a string, a
// constant, or an array of them.
func (jf *javaFile) kafkaTopics(v *sitter.Node) []string {
	if v == nil {
		return nil
	}
	if v.Type() == "element_value_array_initializer" {
		var out []string
		for _, c := range namedChildren(v) {
			out = append(out, jf.kafkaTopics(c)...)
		}
		return out
	}
	t := jf.template(v)
	if t == "" || t == "*" || strings.HasPrefix(t, "#{") {
		return nil // SpEL: computed at run time
	}
	return []string{t}
}

// kafkaContract builds a Kafka contract for a topic. A topic from
// configuration (a placeholder) or with unknown parts is not exact.
func kafkaContract(role, topic, symbol, group string, ln int) store.Contract {
	return store.Contract{Kind: store.ContractKafka, Role: role, Name: topic, Symbol: symbol, Line: ln, Detail: group,
		Exact: !strings.Contains(topic, "*") && !strings.Contains(topic, "${")}
}

// kafkaListener returns the topics an @KafkaListener on a method or class
// consumes.
func (jf *javaFile) kafkaListener(n *sitter.Node, symbol string) []store.Contract {
	a, ok := jf.annotation(modifiersOf(n), "KafkaListener")
	if !ok {
		return nil
	}
	var topics []string
	group := ""
	for _, p := range namedChildren(field(a, "arguments")) {
		if p.Type() != "element_value_pair" {
			continue
		}
		switch text(field(p, "key"), jf.src) {
		case "topics":
			topics = append(topics, jf.kafkaTopics(field(p, "value"))...)
		case "groupId":
			if g := jf.template(field(p, "value")); !strings.Contains(g, "*") {
				group = g
			}
		}
	}
	var out []store.Contract
	for _, t := range topics {
		out = append(out, kafkaContract(store.RoleProvides, t, symbol, group, line(n)))
	}
	return out
}

// valuePlaceholder returns the property placeholder of an @Value field
// ("${orders.topic}"), for evaluating topics built from it.
func (jf *javaFile) valuePlaceholder(m *sitter.Node) string {
	a, ok := jf.annotation(modifiersOf(m), "Value")
	if !ok {
		return ""
	}
	for _, c := range namedChildren(field(a, "arguments")) {
		if c.Type() == "string_literal" {
			if v := jf.template(c); strings.HasPrefix(v, "${") {
				return v
			}
		}
	}
	return ""
}

// kafkaCall recognizes a Kafka producer's send or a consumer's subscribe.
func (jf *javaFile) kafkaCall(n *sitter.Node, locals, fields map[string]string, caller string) []store.Contract {
	obj := field(n, "object")
	name := text(field(n, "name"), jf.src)
	args := namedChildren(field(n, "arguments"))
	if obj == nil || len(args) == 0 || (name != "send" && name != "sendDefault" && name != "subscribe") {
		return nil
	}
	x := ""
	switch {
	case obj.Type() == "identifier":
		x = text(obj, jf.src)
	case obj.Type() == "field_access" && field(obj, "object") != nil && field(obj, "object").Type() == "this":
		x = text(field(obj, "field"), jf.src)
	default:
		return nil
	}
	ref := locals[x]
	if ref == "" {
		ref = fields[x]
	}
	typ := ref[strings.LastIndexAny(ref, ".@~?^|")+1:]
	l := line(n)
	// record returns the topic of new ProducerRecord<>(topic, ...).
	record := func(a *sitter.Node) (string, bool) {
		if a.Type() != "object_creation_expression" || !strings.HasPrefix(text(field(a, "type"), jf.src), "ProducerRecord") {
			return "", false
		}
		if ra := namedChildren(field(a, "arguments")); len(ra) > 0 {
			return jf.template(ra[0]), true
		}
		return "", false
	}
	switch {
	case name == "send" && strings.HasSuffix(typ, "KafkaTemplate"):
		topic, ok := record(args[0])
		if !ok {
			topic = jf.template(args[0])
		}
		if topic != "" && topic != "*" {
			return []store.Contract{kafkaContract(store.RoleUses, topic, caller, "", l)}
		}
	case name == "sendDefault" && strings.HasSuffix(typ, "KafkaTemplate"):
		// The template's default topic: with Spring Boot, this property.
		return []store.Contract{kafkaContract(store.RoleUses, "${spring.kafka.template.default-topic}", caller, "", l)}
	case name == "send" && strings.HasSuffix(typ, "Producer"):
		if topic, ok := record(args[0]); ok && topic != "*" {
			return []store.Contract{kafkaContract(store.RoleUses, topic, caller, "", l)}
		}
	case name == "subscribe" && strings.HasSuffix(typ, "Consumer"):
		list := args[0]
		var topics []*sitter.Node
		if list.Type() == "method_invocation" {
			switch text(field(list, "name"), jf.src) {
			case "of", "asList", "singletonList", "singleton":
				topics = namedChildren(field(list, "arguments"))
			}
		}
		var out []store.Contract
		for _, t := range topics {
			if v := jf.template(t); v != "" && v != "*" {
				out = append(out, kafkaContract(store.RoleProvides, v, caller, "", l))
			}
		}
		return out
	}
	return nil
}

// lambdaParam types the parameter of a lambda passed to
// kafkaTemplate.executeInTransaction(t -> ...): the template itself.
func (jf *javaFile) lambdaParam(lambda *sitter.Node, locals, fields map[string]string) {
	args := lambda.Parent()
	if args == nil || args.Type() != "argument_list" {
		return
	}
	call := args.Parent()
	if call == nil || call.Type() != "method_invocation" || text(field(call, "name"), jf.src) != "executeInTransaction" {
		return
	}
	obj := field(call, "object")
	x := ""
	switch {
	case obj == nil:
		return
	case obj.Type() == "identifier":
		x = text(obj, jf.src)
	case obj.Type() == "field_access" && field(obj, "object") != nil && field(obj, "object").Type() == "this":
		x = text(field(obj, "field"), jf.src)
	}
	ref := locals[x]
	if ref == "" {
		ref = fields[x]
	}
	if !strings.HasSuffix(ref, "KafkaTemplate") {
		return
	}
	switch p := field(lambda, "parameters"); {
	case p == nil:
	case p.Type() == "identifier":
		locals[text(p, jf.src)] = ref
	default:
		if ps := namedChildren(p); len(ps) == 1 && ps[0].Type() == "identifier" {
			locals[text(ps[0], jf.src)] = ref
		}
	}
}
