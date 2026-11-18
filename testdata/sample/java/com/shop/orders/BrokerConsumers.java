package com.shop.orders;

import com.google.cloud.pubsub.v1.Subscriber;
import com.google.cloud.pubsub.v1.SubscriptionAdminClient;
import com.google.pubsub.v1.ProjectSubscriptionName;
import com.google.pubsub.v1.SubscriptionName;
import com.google.pubsub.v1.TopicName;
import io.awspring.cloud.sqs.annotation.SqsListener;
import org.springframework.amqp.rabbit.annotation.Exchange;
import org.springframework.amqp.rabbit.annotation.Queue;
import org.springframework.amqp.rabbit.annotation.QueueBinding;
import org.springframework.amqp.rabbit.annotation.RabbitListener;

/** Consumers of the orders service's queues and subscriptions. */
public class BrokerConsumers {
    @RabbitListener(bindings = @QueueBinding(
        value = @Queue("billing-orders"),
        exchange = @Exchange(value = "orders", type = "topic"),
        key = "order.*"))
    public void bill(String order) {}

    @RabbitListener(queues = "emails")
    public void email(String message) {}

    @SqsListener("shipments")
    public void ship(String id) {}

    void auditInvoices(SubscriptionAdminClient admin, String project) {
        admin.createSubscription(SubscriptionName.of(project, "invoices-audit"), TopicName.of(project, "invoices"), null, 10);
        Subscriber.newBuilder(ProjectSubscriptionName.of(project, "invoices-audit"), null).build();
    }
}
