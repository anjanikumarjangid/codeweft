package com.shop.orders;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.kafka.core.KafkaTemplate;

/** Order events on Kafka. */
public class OrderEvents {
    static final String SETTLED = "payments.settled";

    private final KafkaTemplate<String, String> kafka;

    @Value("${inventory.topic}")
    private String inventoryTopic;

    OrderEvents(KafkaTemplate<String, String> kafka) {
        this.kafka = kafka;
    }

    @KafkaListener(topics = "orders.placed", groupId = "billing")
    public void onOrderPlaced(String orderId) {
        kafka.send(SETTLED, orderId);
    }

    void requestRestock(String sku) {
        this.kafka.send(inventoryTopic, sku);
    }

    @KafkaListener(topics = {"${inventory.topic}"}, groupId = "audit")
    public void audit(String message) {}
}
