package indexer

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Message brokers, TypeScript and JavaScript. Each library is recognized
// only in modules importing it.
//
//	Kafka     kafkajs: kafka.producer() ... producer.send({ topic }),
//	          kafka.consumer({ groupId }) ... consumer.subscribe({ topic | topics })
//	RabbitMQ  amqplib: channel.publish(exchange, key, ...), sendToQueue(queue, ...),
//	          consume(queue, ...), bindQueue(queue, exchange, pattern)
//	SQS       @aws-sdk/client-sqs: new SendMessageCommand({ QueueUrl }), SendMessageBatchCommand,
//	          ReceiveMessageCommand
//	Pub/Sub   @google-cloud/pubsub: pubsub.topic("t").publishMessage(...) or publish(...),
//	          pubsub.subscription("s").on("message", ...), topic("t").createSubscription("s"),
//	          pubsub.createSubscription("t", "s")

func (tf *tsFile) importsModule(prefix string) bool {
	for _, imp := range tf.imports {
		if imp.module == prefix || strings.HasPrefix(imp.module, prefix+"/") {
			return true
		}
	}
	return false
}

// brokerClient reports what a variable is assigned: a kafkajs producer
// ("P") or consumer ("C" and its group), a Pub/Sub topic ("T" and its
// name) or subscription ("S" and its name); "" for anything else.
func (tf *tsFile) brokerClient(value *sitter.Node, clients map[string]string) string {
	for value != nil && value.Type() == "await_expression" {
		value = value.NamedChild(0)
	}
	if value == nil || value.Type() != "call_expression" {
		return ""
	}
	fn := field(value, "function")
	if fn == nil || fn.Type() != "member_expression" {
		return ""
	}
	args := namedChildren(field(value, "arguments"))
	switch prop := text(field(fn, "property"), tf.src); {
	case tf.importsModule("kafkajs") && prop == "producer":
		return "P"
	case tf.importsModule("kafkajs") && prop == "consumer":
		group := ""
		if len(args) > 0 {
			if g := tf.template(tsProperty(args[0], "groupId", tf.src)); !strings.Contains(g, "*") {
				group = g
			}
		}
		return "C" + group
	case tf.importsModule("@google-cloud/pubsub") && prop == "topic" && len(args) > 0:
		return "T" + tf.template(args[0])
	case tf.importsModule("@google-cloud/pubsub") && prop == "subscription" && len(args) > 0:
		return "S" + tf.template(args[0])
	}
	return ""
}

func (tf *tsFile) contract(kind, role, name, detail, caller string, n *sitter.Node) []store.Contract {
	if name == "" || name == "*" {
		return nil
	}
	return []store.Contract{{Kind: kind, Role: role, Name: name, Detail: detail, Symbol: caller, Line: line(n),
		Exact: !strings.Contains(name+detail, "*") && !strings.Contains(name+detail, "${")}}
}

// messagingCall recognizes a call producing to, consuming from or binding a
// broker's topic or queue.
func (tf *tsFile) messagingCall(n *sitter.Node, caller string, clients map[string]string) []store.Contract {
	fn := field(n, "function")
	if fn == nil || fn.Type() != "member_expression" {
		return nil
	}
	obj, prop := field(fn, "object"), text(field(fn, "property"), tf.src)
	args := namedChildren(field(n, "arguments"))
	// held is what the receiver holds: a client variable, or a call such as
	// pubsub.topic("t") written in place.
	held := ""
	if obj != nil && obj.Type() == "identifier" {
		held = clients[text(obj, tf.src)]
	} else if obj != nil {
		held = tf.brokerClient(obj, clients)
	}
	var out []store.Contract
	switch {
	case strings.HasPrefix(held, "P") && (prop == "send" || prop == "sendBatch") && len(args) > 0:
		out = append(out, tf.contract(store.ContractKafka, store.RoleUses, tf.template(tsProperty(args[0], "topic", tf.src)), "", caller, n)...)
	case strings.HasPrefix(held, "C") && prop == "subscribe" && len(args) > 0:
		group := held[1:]
		if t := tsProperty(args[0], "topic", tf.src); t != nil {
			out = append(out, tf.contract(store.ContractKafka, store.RoleProvides, tf.template(t), group, caller, n)...)
		}
		if ts := tsProperty(args[0], "topics", tf.src); ts != nil && ts.Type() == "array" {
			for _, t := range namedChildren(ts) {
				out = append(out, tf.contract(store.ContractKafka, store.RoleProvides, tf.template(t), group, caller, n)...)
			}
		}
	case strings.HasPrefix(held, "T") && (prop == "publishMessage" || prop == "publish" || prop == "publishJSON"):
		out = append(out, tf.contract(store.ContractPubSub, store.RoleUses, held[1:], "", caller, n)...)
	case strings.HasPrefix(held, "T") && prop == "createSubscription" && len(args) > 0:
		out = append(out, tf.contract(store.ContractPubSub, store.RoleBinds, held[1:], tf.template(args[0]), caller, n)...)
	case strings.HasPrefix(held, "S") && prop == "on" && len(args) > 0 && tsString(args[0], tf.src) == "message":
		out = append(out, tf.contract(store.ContractPubSub, store.RoleProvides, held[1:], "", caller, n)...)
	case tf.importsModule("@google-cloud/pubsub") && prop == "createSubscription" && len(args) >= 2:
		out = append(out, tf.contract(store.ContractPubSub, store.RoleBinds, tf.template(args[0]), tf.template(args[1]), caller, n)...)
	case tf.importsModule("amqplib") && prop == "publish" && len(args) >= 3:
		ex, key := tf.template(args[0]), tf.template(args[1])
		if ex != "" {
			key = ex + "/" + key
		}
		out = append(out, tf.contract(store.ContractAMQP, store.RoleUses, key, "", caller, n)...)
	case tf.importsModule("amqplib") && prop == "sendToQueue" && len(args) >= 2:
		out = append(out, tf.contract(store.ContractAMQP, store.RoleUses, tf.template(args[0]), "", caller, n)...)
	case tf.importsModule("amqplib") && prop == "consume" && len(args) >= 2:
		out = append(out, tf.contract(store.ContractAMQP, store.RoleProvides, tf.template(args[0]), "", caller, n)...)
	case tf.importsModule("amqplib") && prop == "bindQueue" && len(args) >= 3:
		if q, ex := tf.template(args[0]), tf.template(args[1]); q != "*" && ex != "*" {
			out = append(out, tf.contract(store.ContractAMQP, store.RoleBinds, ex+"/"+tf.template(args[2]), q, caller, n)...)
		}
	}
	return out
}

// messagingNew recognizes an SQS command: new SendMessageCommand({ QueueUrl }).
func (tf *tsFile) messagingNew(n *sitter.Node, caller string) []store.Contract {
	if !tf.importsModule("@aws-sdk/client-sqs") {
		return nil
	}
	args := namedChildren(field(n, "arguments"))
	if len(args) == 0 {
		return nil
	}
	url := store.SQSQueueName(tf.template(tsProperty(args[0], "QueueUrl", tf.src)))
	switch text(field(n, "constructor"), tf.src) {
	case "SendMessageCommand", "SendMessageBatchCommand":
		return tf.contract(store.ContractSQS, store.RoleUses, url, "", caller, n)
	case "ReceiveMessageCommand":
		return tf.contract(store.ContractSQS, store.RoleProvides, url, "", caller, n)
	}
	return nil
}
