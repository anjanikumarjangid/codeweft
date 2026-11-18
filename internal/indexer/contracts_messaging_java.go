package indexer

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Message brokers, Java.
//
//	RabbitMQ  Spring AMQP: @RabbitListener(queues = ..., queuesToDeclare = @Queue(...),
//	          bindings = @QueueBinding(value = @Queue(...), exchange = @Exchange(...), key = ...));
//	          rabbitTemplate.convertAndSend([exchange,] routingKey, message), send(exchange, key, message)
//	SQS       Spring Cloud AWS: @SqsListener("queue"), sqsTemplate.send("queue", payload);
//	          AWS SDK v2: SendMessageRequest.builder().queueUrl(...), SendMessageBatchRequest,
//	          ReceiveMessageRequest
//	Pub/Sub   Publisher.newBuilder(TopicName.of(project, "t")), Subscriber.newBuilder(
//	          ProjectSubscriptionName.of(project, "s"), receiver), createSubscription(subscription,
//	          topic, ...); Spring Cloud GCP: pubSubTemplate.publish("t", ...), subscribe("s", ...)

// annotationValue returns the value of an annotation's element: key = ...,
// or the single unnamed argument for "value".
func (jf *javaFile) annotationValue(a *sitter.Node, key string) *sitter.Node {
	for _, c := range namedChildren(field(a, "arguments")) {
		if c.Type() == "element_value_pair" {
			if text(field(c, "key"), jf.src) == key {
				return field(c, "value")
			}
		} else if key == "value" {
			return c
		}
	}
	return nil
}

// annotationStrings evaluates an annotation element to its strings: a
// string or constant, an array of them, or nested annotations' values
// (queuesToDeclare = @Queue("orders")).
func (jf *javaFile) annotationStrings(v *sitter.Node) []string {
	if v == nil {
		return nil
	}
	switch v.Type() {
	case "element_value_array_initializer":
		var out []string
		for _, c := range namedChildren(v) {
			out = append(out, jf.annotationStrings(c)...)
		}
		return out
	case "annotation", "marker_annotation":
		name := jf.annotationValue(v, "value")
		if name == nil {
			name = jf.annotationValue(v, "name")
		}
		return jf.annotationStrings(name)
	}
	if s := jf.template(v); s != "" && s != "*" && !strings.HasPrefix(s, "#{") {
		return []string{s}
	}
	return nil
}

// brokerListeners returns what a @RabbitListener or @SqsListener on a
// method or class consumes, and the bindings it declares.
func (jf *javaFile) brokerListeners(n *sitter.Node, symbol string) []store.Contract {
	mods := modifiersOf(n)
	var out []store.Contract
	add := func(kind, role, name, detail string) {
		out = append(out, store.Contract{Kind: kind, Role: role, Name: name, Detail: detail, Symbol: symbol, Line: line(n),
			Exact: !strings.Contains(name+detail, "${")})
	}
	if a, ok := jf.annotation(mods, "RabbitListener"); ok {
		for _, key := range []string{"queues", "queuesToDeclare"} {
			for _, q := range jf.annotationStrings(jf.annotationValue(a, key)) {
				add(store.ContractAMQP, store.RoleProvides, q, "")
			}
		}
		var bindings []*sitter.Node
		if b := jf.annotationValue(a, "bindings"); b != nil {
			bindings = append(bindings, b)
			if b.Type() == "element_value_array_initializer" {
				bindings = namedChildren(b)
			}
		}
		for _, b := range bindings {
			if b.Type() != "annotation" {
				continue
			}
			queues := jf.annotationStrings(jf.annotationValue(b, "value"))
			exchanges := jf.annotationStrings(jf.annotationValue(b, "exchange"))
			keys := jf.annotationStrings(jf.annotationValue(b, "key"))
			if len(keys) == 0 {
				keys = []string{""}
			}
			for _, q := range queues {
				add(store.ContractAMQP, store.RoleProvides, q, "")
				for _, ex := range exchanges {
					for _, k := range keys {
						add(store.ContractAMQP, store.RoleBinds, ex+"/"+k, q)
					}
				}
			}
		}
	}
	if a, ok := jf.annotation(mods, "SqsListener"); ok {
		for _, key := range []string{"value", "queueNames"} {
			for _, q := range jf.annotationStrings(jf.annotationValue(a, key)) {
				add(store.ContractSQS, store.RoleProvides, store.SQSQueueName(q), "")
			}
		}
	}
	return out
}

// javaBrokerName evaluates a queue, topic or subscription name: a string or
// constant, TopicName.of(project, "t") and the like, or a written-out path
// ("projects/p/topics/t").
func (jf *javaFile) javaBrokerName(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	if n.Type() == "method_invocation" && text(field(n, "name"), jf.src) == "of" {
		switch o := text(field(n, "object"), jf.src); o[strings.LastIndex(o, ".")+1:] {
		case "TopicName", "ProjectTopicName", "SubscriptionName", "ProjectSubscriptionName":
			if args := namedChildren(field(n, "arguments")); len(args) == 2 {
				return jf.javaBrokerName(args[1])
			}
			return ""
		}
	}
	v := jf.template(n)
	if v == "*" {
		return ""
	}
	for _, seg := range []string{"/topics/", "/subscriptions/"} {
		if i := strings.LastIndex(v, seg); i >= 0 && strings.HasPrefix(v, "projects/") {
			return v[i+len(seg):]
		}
	}
	return v
}

// messagingCall recognizes a call publishing to, consuming from or binding
// a broker's queue or topic.
func (jf *javaFile) messagingCall(n *sitter.Node, locals, fields map[string]string, caller string) []store.Contract {
	name := text(field(n, "name"), jf.src)
	obj := field(n, "object")
	args := namedChildren(field(n, "arguments"))
	var out []store.Contract
	add := func(kind, role, target, detail string) {
		if target == "" || target == "*" {
			return
		}
		out = append(out, store.Contract{Kind: kind, Role: role, Name: target, Detail: detail, Symbol: caller, Line: line(n),
			Exact: !strings.Contains(target+detail, "*") && !strings.Contains(target+detail, "${")})
	}
	// The type of the receiver, for templates held in fields or locals.
	typ := ""
	if obj != nil {
		x := text(obj, jf.src)
		if obj.Type() == "field_access" && field(obj, "object") != nil && field(obj, "object").Type() == "this" {
			x = text(field(obj, "field"), jf.src)
		}
		ref := locals[x]
		if ref == "" {
			ref = fields[x]
		}
		typ = ref[strings.LastIndexAny(ref, ".@~?^|")+1:]
	}
	isString := func(a *sitter.Node) bool {
		return a.Type() != "lambda_expression" && a.Type() != "object_creation_expression"
	}
	switch {
	case (name == "convertAndSend" || name == "send") && hasAnySuffix(typ, "RabbitTemplate", "AmqpTemplate", "RabbitOperations", "AmqpOperations"):
		switch {
		case len(args) >= 3 && isString(args[0]) && isString(args[1]):
			ex, key := jf.template(args[0]), jf.template(args[1])
			if ex != "" {
				key = ex + "/" + key
			}
			add(store.ContractAMQP, store.RoleUses, key, "")
		case len(args) == 2 && name == "convertAndSend" && args[0].Type() == "string_literal":
			add(store.ContractAMQP, store.RoleUses, jf.template(args[0]), "") // the default exchange
		}
	case name == "send" && hasAnySuffix(typ, "SqsTemplate", "SqsOperations") && len(args) == 2 && isString(args[0]):
		add(store.ContractSQS, store.RoleUses, store.SQSQueueName(jf.template(args[0])), "")
	case name == "publish" && hasAnySuffix(typ, "PubSubTemplate", "PubSubOperations", "PubSubPublisherTemplate") && len(args) >= 2:
		add(store.ContractPubSub, store.RoleUses, jf.javaBrokerName(args[0]), "")
	case name == "subscribe" && hasAnySuffix(typ, "PubSubTemplate", "PubSubOperations", "PubSubSubscriberTemplate") && len(args) >= 2:
		add(store.ContractPubSub, store.RoleProvides, jf.javaBrokerName(args[0]), "")
	case name == "createSubscription" && len(args) >= 2:
		// subscriptionAdminClient.createSubscription(subscription, topic, ...),
		// pubSubAdmin.createSubscription("s", "t")
		if s, t := jf.javaBrokerName(args[0]), jf.javaBrokerName(args[1]); s != "" && t != "" {
			add(store.ContractPubSub, store.RoleBinds, t, s)
		}
	case name == "newBuilder" && obj != nil && len(args) >= 1:
		switch text(obj, jf.src) {
		case "Publisher":
			add(store.ContractPubSub, store.RoleUses, jf.javaBrokerName(args[0]), "")
		case "Subscriber":
			add(store.ContractPubSub, store.RoleProvides, jf.javaBrokerName(args[0]), "")
		}
	case name == "queueUrl" && len(args) == 1:
		// SendMessageRequest.builder().messageBody(...).queueUrl(url)...
		for o := obj; o != nil && o.Type() == "method_invocation"; o = field(o, "object") {
			if text(field(o, "name"), jf.src) != "builder" {
				continue
			}
			url := store.SQSQueueName(jf.template(args[0]))
			switch text(field(o, "object"), jf.src) {
			case "SendMessageRequest", "SendMessageBatchRequest":
				add(store.ContractSQS, store.RoleUses, url, "")
			case "ReceiveMessageRequest":
				add(store.ContractSQS, store.RoleProvides, url, "")
			}
			break
		}
	}
	return out
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, x := range suffixes {
		if s != "" && strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}
