package checkout

import (
	"context"

	"cloud.google.com/go/pubsub"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	amqp "github.com/rabbitmq/amqp091-go"
)

const shipmentsQueue = "https://sqs.us-east-1.amazonaws.com/123456789012/shipments"

// announceOrder publishes a created order on the orders exchange.
func announceOrder(ctx context.Context, ch *amqp.Channel, body []byte) error {
	return ch.PublishWithContext(ctx, "orders", "order.created", false, false, amqp.Publishing{Body: body})
}

// requestShipment queues an order for shipping.
func requestShipment(ctx context.Context, client *sqs.Client, id string) error {
	_, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(shipmentsQueue), MessageBody: aws.String(id)})
	return err
}

// publishInvoice publishes an order's invoice.
func publishInvoice(ctx context.Context, client *pubsub.Client, data []byte) {
	client.Topic("invoices").Publish(ctx, &pubsub.Message{Data: data})
}
