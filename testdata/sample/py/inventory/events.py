"""Inventory events on Kafka."""
from confluent_kafka import Producer
from kafka import KafkaConsumer

STOCK_TOPIC = "stock.changed"


def consume_orders():
    consumer = KafkaConsumer("orders.placed", group_id="inventory")
    for message in consumer:
        print(message)


class StockEvents:
    """Publishes stock changes."""

    def __init__(self, conf):
        self.producer = Producer(conf)

    def publish(self, sku):
        self.producer.produce(STOCK_TOPIC, value=sku)
