package indexer

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Message brokers, Python clients. Each library is recognized only in
// files importing it.
//
//	RabbitMQ  pika: channel.basic_publish(exchange, routing_key, body), basic_consume(queue, ...),
//	          queue_bind(queue, exchange, routing_key)
//	SQS       boto3: client.send_message(QueueUrl=...), send_message_batch, receive_message;
//	          resource queues from get_queue_by_name(QueueName=...) or Queue(url)
//	Pub/Sub   google.cloud.pubsub_v1: publisher.publish(topic_path, ...),
//	          subscriber.subscribe(subscription_path, ...),
//	          subscriber.create_subscription(name=..., topic=...) or request={"name": ..., "topic": ...};
//	          paths from topic_path(project, "t") and subscription_path(project, "s")

// pyImports reports whether the file imports a module with this prefix.
func (pf *pyFile) pyImports(prefix string) bool {
	for _, p := range pf.imports {
		if p == prefix || strings.HasPrefix(p, prefix+".") {
			return true
		}
	}
	return false
}

// pyArgs splits a call's arguments into positional ones and keywords.
func (pf *pyFile) pyArgs(call *sitter.Node) ([]*sitter.Node, map[string]*sitter.Node) {
	var pos []*sitter.Node
	kw := map[string]*sitter.Node{}
	for _, a := range namedChildren(field(call, "arguments")) {
		if a.Type() == "keyword_argument" {
			kw[text(field(a, "name"), pf.src)] = field(a, "value")
		} else {
			pos = append(pos, a)
		}
	}
	return pos, kw
}

// arg returns the argument passed by keyword, or at a position.
func arg(pos []*sitter.Node, kw map[string]*sitter.Node, name string, i int) *sitter.Node {
	if v := kw[name]; v != nil {
		return v
	}
	if i < len(pos) {
		return pos[i]
	}
	return nil
}

// brokerName evaluates a queue, topic or subscription name: a string, a
// constant, a local variable assigned one, a Pub/Sub path built with
// topic_path(project, "t") or written out ("projects/p/topics/t"), or a
// queue URL.
func (pf *pyFile) brokerName(n *sitter.Node, assigned map[string]*sitter.Node, depth int) string {
	if n == nil || depth > 3 {
		return ""
	}
	switch n.Type() {
	case "identifier":
		if v, ok := assigned[text(n, pf.src)]; ok {
			return pf.brokerName(v, assigned, depth+1)
		}
	case "call":
		fn := field(n, "function")
		if fn != nil && fn.Type() == "attribute" {
			switch text(field(fn, "attribute"), pf.src) {
			case "topic_path", "subscription_path":
				if pos, _ := pf.pyArgs(n); len(pos) == 2 {
					return pf.brokerName(pos[1], assigned, depth+1)
				}
				return ""
			}
		}
	}
	v := pf.template(n)
	if v == "*" || v == "" {
		return ""
	}
	if i := strings.LastIndex(v, "/topics/"); i >= 0 && strings.HasPrefix(v, "projects/") {
		return v[i+len("/topics/"):]
	}
	if i := strings.LastIndex(v, "/subscriptions/"); i >= 0 && strings.HasPrefix(v, "projects/") {
		return v[i+len("/subscriptions/"):]
	}
	return v
}

// messagingCall recognizes a broker call. assigned maps the function's
// local variables to the expression last assigned to them; queues maps
// variables holding a boto3 SQS resource queue to its name.
func (pf *pyFile) messagingCall(n *sitter.Node, caller string, assigned map[string]*sitter.Node, queues map[string]string) []store.Contract {
	fn := field(n, "function")
	if fn == nil || fn.Type() != "attribute" {
		return nil
	}
	method := text(field(fn, "attribute"), pf.src)
	obj := field(fn, "object")
	pos, kw := pf.pyArgs(n)
	var out []store.Contract
	add := func(kind, role, name, detail string) {
		if name == "" {
			return
		}
		exact := !strings.Contains(name+detail, "*") && !strings.Contains(name+detail, "${")
		out = append(out, store.Contract{Kind: kind, Role: role, Name: name, Detail: detail, Symbol: caller, Line: line(n), Exact: exact})
	}
	name := func(i int, kws ...string) string {
		for _, k := range kws {
			if v := kw[k]; v != nil {
				return pf.brokerName(v, assigned, 0)
			}
		}
		if i >= 0 && i < len(pos) {
			return pf.brokerName(pos[i], assigned, 0)
		}
		return ""
	}
	switch {
	case pf.pyImports("pika") && method == "basic_publish":
		ex, key := name(0, "exchange"), name(1, "routing_key")
		if ex != "" {
			key = ex + "/" + key
		}
		if !strings.HasSuffix(key, "/") {
			add(store.ContractAMQP, store.RoleUses, key, "")
		}
	case pf.pyImports("pika") && method == "basic_consume":
		add(store.ContractAMQP, store.RoleProvides, name(0, "queue"), "")
	case pf.pyImports("pika") && method == "queue_bind":
		if q, ex := name(0, "queue"), name(1, "exchange"); q != "" && ex != "" {
			add(store.ContractAMQP, store.RoleBinds, ex+"/"+name(2, "routing_key"), q)
		}
	case pf.pyImports("boto3") && (method == "send_message" || method == "send_message_batch"):
		if url := name(-1, "QueueUrl"); url != "" {
			add(store.ContractSQS, store.RoleUses, store.SQSQueueName(url), "")
		} else if obj != nil && queues[text(obj, pf.src)] != "" {
			add(store.ContractSQS, store.RoleUses, queues[text(obj, pf.src)], "")
		}
	case pf.pyImports("boto3") && (method == "receive_message" || method == "receive_messages"):
		if url := name(-1, "QueueUrl"); url != "" {
			add(store.ContractSQS, store.RoleProvides, store.SQSQueueName(url), "")
		} else if obj != nil && queues[text(obj, pf.src)] != "" {
			add(store.ContractSQS, store.RoleProvides, queues[text(obj, pf.src)], "")
		}
	case pf.pyImports("google.cloud.pubsub_v1") || pf.pyImports("google.cloud.pubsub"):
		switch method {
		case "publish":
			add(store.ContractPubSub, store.RoleUses, name(0, "topic"), "")
		case "subscribe":
			add(store.ContractPubSub, store.RoleProvides, name(0, "subscription"), "")
		case "create_subscription":
			sub, topic := name(0, "name"), name(1, "topic")
			if req := kw["request"]; req != nil && req.Type() == "dictionary" {
				for _, pair := range namedChildren(req) {
					if pair.Type() != "pair" {
						continue
					}
					switch strings.Trim(text(field(pair, "key"), pf.src), `"'`) {
					case "name":
						sub = pf.brokerName(field(pair, "value"), assigned, 0)
					case "topic":
						topic = pf.brokerName(field(pair, "value"), assigned, 0)
					}
				}
			}
			if sub != "" && topic != "" {
				add(store.ContractPubSub, store.RoleBinds, topic, sub)
			}
		}
	}
	return out
}

// sqsQueue returns the name of the queue a boto3 resource call gives:
// sqs.get_queue_by_name(QueueName="orders"), sqs.Queue(url).
func (pf *pyFile) sqsQueue(call *sitter.Node, assigned map[string]*sitter.Node) string {
	if call == nil || call.Type() != "call" || !pf.pyImports("boto3") {
		return ""
	}
	fn := field(call, "function")
	if fn == nil || fn.Type() != "attribute" {
		return ""
	}
	pos, kw := pf.pyArgs(call)
	switch text(field(fn, "attribute"), pf.src) {
	case "get_queue_by_name":
		return pf.brokerName(arg(pos, kw, "QueueName", 0), assigned, 0)
	case "Queue":
		return store.SQSQueueName(pf.brokerName(arg(pos, kw, "url", 0), assigned, 0))
	}
	return ""
}
