package indexer

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Message brokers, Go clients. Each library is recognized only in files
// importing it.
//
//	RabbitMQ  amqp091-go, streadway/amqp:
//	          ch.Publish(exchange, key, ...), ch.PublishWithContext(ctx, exchange, key, ...)
//	          ch.Consume(queue, ...)
//	          ch.QueueBind(queue, key, exchange, ...)
//	SQS       aws-sdk-go (v1 and v2): sqs.SendMessageInput{QueueUrl}, SendMessageBatchInput,
//	          ReceiveMessageInput{QueueUrl}
//	Pub/Sub   cloud.google.com/go/pubsub: client.Topic("t").Publish, client.Publisher("t").Publish,
//	          client.Subscription("s").Receive, client.Subscriber("s").Receive,
//	          client.CreateSubscription(ctx, "s", pubsub.SubscriptionConfig{Topic: client.Topic("t")})

func goImports(aliases map[string]string, match func(path string) bool) (names map[string]bool) {
	names = map[string]bool{}
	for a, p := range aliases {
		if match(p) {
			names[a] = true
		}
	}
	return names
}

func goMessagingCalls(fset *token.FileSet, body *ast.BlockStmt, caller string, aliases, consts map[string]string, enc *goEncoder) []store.Contract {
	amqp := goImports(aliases, func(p string) bool {
		return p == "github.com/rabbitmq/amqp091-go" || p == "github.com/streadway/amqp"
	})
	sqs := goImports(aliases, func(p string) bool {
		return p == "github.com/aws/aws-sdk-go-v2/service/sqs" || p == "github.com/aws/aws-sdk-go/service/sqs"
	})
	pubsub := goImports(aliases, func(p string) bool {
		return p == "cloud.google.com/go/pubsub" || p == "cloud.google.com/go/pubsub/v2"
	})
	if len(amqp)+len(sqs)+len(pubsub) == 0 {
		return nil
	}
	// str evaluates a string argument; aws.String(x) is x.
	var str func(e ast.Expr) string
	str = func(e ast.Expr) string {
		if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "String" {
				return str(call.Args[0])
			}
		}
		v := goString(e, consts, aliases, enc, 0)
		if v == "*" {
			return ""
		}
		return v
	}
	var out []store.Contract
	add := func(kind, role, name, detail string, n ast.Node) {
		if name == "" || name == "*" {
			return // nothing of the name is known
		}
		exact := !strings.Contains(name+detail, "*") && !strings.Contains(name+detail, "${")
		out = append(out, store.Contract{Kind: kind, Role: role, Name: name, Detail: detail, Symbol: caller,
			Line: fset.Position(n.Pos()).Line, Exact: exact})
	}
	// pubsubName returns the topic or subscription id of client.Topic("t")
	// (or Publisher, Subscription, Subscriber), following a local variable.
	var pubsubName func(e ast.Expr, methods ...string) string
	pubsubName = func(e ast.Expr, methods ...string) string {
		switch t := e.(type) {
		case *ast.CallExpr:
			if sel, ok := t.Fun.(*ast.SelectorExpr); ok && len(t.Args) == 1 {
				for _, m := range methods {
					if sel.Sel.Name == m {
						return str(t.Args[0])
					}
				}
			}
		case *ast.Ident:
			if l, ok := enc.lookup(t.Name, t.Pos()); ok && l.val != nil {
				return pubsubName(l.val, methods...)
			}
		}
		return ""
	}
	litPkg := func(lit *ast.CompositeLit) (string, string) {
		if sel, ok := lit.Type.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok {
				return pkg.Name, sel.Sel.Name
			}
		}
		return "", ""
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.CompositeLit:
			pkg, typ := litPkg(t)
			if !sqs[pkg] {
				return true
			}
			url := str(goCompositeField(t, "QueueUrl", enc))
			switch typ {
			case "SendMessageInput", "SendMessageBatchInput":
				add(store.ContractSQS, store.RoleUses, store.SQSQueueName(url), "", t)
			case "ReceiveMessageInput":
				add(store.ContractSQS, store.RoleProvides, store.SQSQueueName(url), "", t)
			}
		case *ast.CallExpr:
			sel, ok := t.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && enc.isPackage(pkg.Name, pkg.Pos()) {
				return true
			}
			args := t.Args
			switch name := sel.Sel.Name; {
			case len(amqp) > 0 && name == "Publish" && len(args) == 5,
				len(amqp) > 0 && name == "PublishWithContext" && len(args) == 6:
				if name == "PublishWithContext" {
					args = args[1:]
				}
				ex, key := str(args[0]), str(args[1])
				target := key // the default exchange routes to the queue named by the key
				if ex != "" {
					target = ex + "/" + key
				}
				add(store.ContractAMQP, store.RoleUses, target, "", t)
			case len(amqp) > 0 && name == "Consume" && len(args) == 7:
				add(store.ContractAMQP, store.RoleProvides, str(args[0]), "", t)
			case len(amqp) > 0 && name == "QueueBind" && len(args) == 5:
				if q, ex := str(args[0]), str(args[2]); q != "" && ex != "" {
					add(store.ContractAMQP, store.RoleBinds, ex+"/"+str(args[1]), q, t)
				}
			case len(pubsub) > 0 && name == "Publish" && len(args) == 2:
				add(store.ContractPubSub, store.RoleUses, pubsubName(sel.X, "Topic", "Publisher"), "", t)
			case len(pubsub) > 0 && name == "Receive" && len(args) == 2:
				add(store.ContractPubSub, store.RoleProvides, pubsubName(sel.X, "Subscription", "Subscriber"), "", t)
			case len(pubsub) > 0 && name == "CreateSubscription" && len(args) == 3:
				if lit, ok := unparen(args[2]).(*ast.CompositeLit); ok {
					if topic := pubsubName(goCompositeField(lit, "Topic", enc), "Topic"); topic != "" {
						add(store.ContractPubSub, store.RoleBinds, topic, str(args[1]), t)
					}
				}
			}
		}
		return true
	})
	return out
}

func unparen(e ast.Expr) ast.Expr {
	for {
		switch t := e.(type) {
		case *ast.ParenExpr:
			e = t.X
		case *ast.UnaryExpr:
			if t.Op != token.AND {
				return e
			}
			e = t.X
		default:
			return e
		}
	}
}
