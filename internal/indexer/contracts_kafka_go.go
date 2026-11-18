package indexer

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// Kafka, Go clients. A topic is provided by the code consuming it and used
// by the code producing to it: a link goes from a producer to every
// consumer of its topic.
//
//	segmentio/kafka-go  producer: kafka.Writer{Topic}, kafka.Message{Topic}
//	                    consumer: kafka.ReaderConfig{Topic | GroupTopics, GroupID}
//	sarama              producer: sarama.ProducerMessage{Topic}
//	                    consumer: ConsumePartition(topic, ...), group.Consume(ctx, topics, handler)
//	confluent-kafka-go  producer: kafka.Message{TopicPartition: kafka.TopicPartition{Topic: &topic}}
//	                    consumer: Subscribe(topic, cb), SubscribeTopics(topics, cb)
//	franz-go            producer: kgo.Record{Topic}
//	                    consumer: kgo.ConsumeTopics(topics...)

const (
	kafkaSegmentio = "github.com/segmentio/kafka-go"
	kafkaFranz     = "github.com/twmb/franz-go/pkg/kgo"
)

func isSarama(p string) bool {
	return p == "github.com/IBM/sarama" || p == "github.com/Shopify/sarama"
}

func isConfluent(p string) bool {
	return strings.HasPrefix(p, "github.com/confluentinc/confluent-kafka-go") && strings.HasSuffix(p, "/kafka")
}

// goKafkaLib returns the Kafka client library a package alias names, if any.
func goKafkaLib(alias string, aliases map[string]string) string {
	switch p := aliases[alias]; {
	case p == kafkaSegmentio:
		return "segmentio"
	case isSarama(p):
		return "sarama"
	case isConfluent(p):
		return "confluent"
	case p == kafkaFranz:
		return "franz"
	}
	return ""
}

// goString evaluates a string expression as goTemplate does, also
// following local variables and & (Topic: &topic with topic := "orders").
func goString(e ast.Expr, consts, aliases map[string]string, enc *goEncoder, depth int) string {
	switch t := e.(type) {
	case *ast.UnaryExpr:
		if t.Op == token.AND {
			return goString(t.X, consts, aliases, enc, depth)
		}
	case *ast.Ident:
		if _, ok := consts[t.Name]; !ok && depth < 3 {
			if l, ok := enc.lookup(t.Name, t.Pos()); ok && l.val != nil {
				return goString(l.val, consts, aliases, enc, depth+1)
			}
		}
	}
	return goTemplate(e, consts, aliases)
}

// goKafkaCalls finds the topics a function body produces to and consumes.
func goKafkaCalls(fset *token.FileSet, body *ast.BlockStmt, caller, qual string, aliases, consts map[string]string, enc *goEncoder) []store.Contract {
	libs := map[string]bool{}
	for a := range aliases {
		if l := goKafkaLib(a, aliases); l != "" {
			libs[l] = true
		}
	}
	if len(libs) == 0 {
		return nil
	}
	var out []store.Contract
	topic := func(role string, e ast.Expr, line int, symbol, group string) {
		if e == nil {
			return
		}
		name := goString(e, consts, aliases, enc, 0)
		if name == "" || name == "*" {
			return
		}
		if symbol == "" {
			symbol = caller
		}
		out = append(out, store.Contract{Kind: store.ContractKafka, Role: role, Name: name, Symbol: symbol,
			Line: line, Detail: group, Exact: !strings.Contains(name, "*") && !strings.Contains(name, "${")})
	}
	topics := func(role string, e ast.Expr, line int, symbol, group string) {
		// topics := []string{topic}; SubscribeTopics(topics, nil)
		if id, ok := e.(*ast.Ident); ok {
			if l, ok := enc.lookup(id.Name, id.Pos()); ok && l.val != nil {
				if _, isLit := l.val.(*ast.CompositeLit); isLit {
					e = l.val
				}
			}
		}
		if lit, ok := e.(*ast.CompositeLit); ok {
			if _, isSlice := lit.Type.(*ast.ArrayType); isSlice {
				for _, el := range lit.Elts {
					topic(role, el, line, symbol, group)
				}
				return
			}
		}
		topic(role, e, line, symbol, group)
	}
	// litLib returns the library and type name of a composite literal's
	// type written as pkg.Type.
	litLib := func(lit *ast.CompositeLit) (string, string) {
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return "", ""
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return "", ""
		}
		return goKafkaLib(pkg.Name, aliases), sel.Sel.Name
	}
	field := func(lit *ast.CompositeLit, name string) ast.Expr { return goCompositeField(lit, name, enc) }
	ast.Inspect(body, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.CompositeLit:
			line := fset.Position(t.Pos()).Line
			switch lib, typ := litLib(t); {
			case lib == "segmentio" && (typ == "Writer" || typ == "Message"):
				topic(store.RoleUses, field(t, "Topic"), line, "", "")
			case lib == "segmentio" && typ == "ReaderConfig":
				group := goString(field(t, "GroupID"), consts, aliases, enc, 0)
				if strings.Contains(group, "*") {
					group = ""
				}
				topic(store.RoleProvides, field(t, "Topic"), line, "", group)
				if gt := field(t, "GroupTopics"); gt != nil {
					topics(store.RoleProvides, gt, line, "", group)
				}
			case lib == "sarama" && typ == "ProducerMessage":
				topic(store.RoleUses, field(t, "Topic"), line, "", "")
			case lib == "confluent" && typ == "TopicPartition":
				// In a message being produced; a consumer assigning
				// partitions builds TopicPartitions outside a Message.
				return false
			case lib == "confluent" && typ == "Message":
				if tp, ok := field(t, "TopicPartition").(*ast.CompositeLit); ok {
					topic(store.RoleUses, field(tp, "Topic"), line, "", "")
				}
			case lib == "franz" && typ == "Record":
				topic(store.RoleUses, field(t, "Topic"), line, "", "")
			}
		case *ast.CallExpr:
			sel, ok := t.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			line := fset.Position(t.Pos()).Line
			pkgLib := ""
			if pkg, ok := sel.X.(*ast.Ident); ok && enc.isPackage(pkg.Name, pkg.Pos()) {
				pkgLib = goKafkaLib(pkg.Name, aliases)
				if pkgLib == "" {
					return true
				}
			}
			switch name, args := sel.Sel.Name, t.Args; {
			case pkgLib == "franz" && name == "ConsumeTopics":
				for _, a := range args {
					topic(store.RoleProvides, a, line, "", "")
				}
			case pkgLib != "":
			case libs["sarama"] && name == "ConsumePartition" && len(args) == 3:
				topic(store.RoleProvides, args[0], line, "", "")
			case libs["sarama"] && name == "Consume" && len(args) == 3:
				// A consumer group: the handler's ConsumeClaim consumes.
				symbol := ""
				if typ := goStructValue(args[2], enc, 0); typ != nil {
					if ref := goTypeRef(typ, qual, aliases); ref != "" {
						symbol = ref + ".ConsumeClaim"
					}
				}
				topics(store.RoleProvides, args[1], line, symbol, "")
			case libs["confluent"] && name == "Subscribe" && len(args) == 2:
				topic(store.RoleProvides, args[0], line, "", "")
			case libs["confluent"] && name == "SubscribeTopics" && len(args) == 2:
				topics(store.RoleProvides, args[0], line, "", "")
			}
		}
		return true
	})
	return out
}
