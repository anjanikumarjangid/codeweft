package checkout

import (
	"context"

	"github.com/IBM/sarama"
	confluent "github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/segmentio/kafka-go"
)

// orderTopic is where placed orders are announced.
const orderTopic = "orders.placed"

// publishOrderPlaced announces a placed order.
func publishOrderPlaced(ctx context.Context, id string) error {
	w := &kafka.Writer{Addr: kafka.TCP("kafka:9092"), Topic: orderTopic}
	return w.WriteMessages(ctx, kafka.Message{Key: []byte(id)})
}

// paymentHandler marks orders paid when their payment settles.
type paymentHandler struct{}

func (paymentHandler) Setup(sarama.ConsumerGroupSession) error   { return nil }
func (paymentHandler) Cleanup(sarama.ConsumerGroupSession) error { return nil }

// ConsumeClaim reads settled payments.
func (paymentHandler) ConsumeClaim(s sarama.ConsumerGroupSession, c sarama.ConsumerGroupClaim) error {
	return nil
}

// consumePayments runs the payment consumer group.
func consumePayments(ctx context.Context, group sarama.ConsumerGroup) error {
	return group.Consume(ctx, []string{"payments.settled"}, paymentHandler{})
}

// watchStock follows stock changes from the inventory service.
func watchStock(c *confluent.Consumer) error {
	return c.SubscribeTopics([]string{"stock.changed"}, nil)
}
