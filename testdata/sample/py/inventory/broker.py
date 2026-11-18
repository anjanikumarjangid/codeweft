"""Inventory's queues and subscriptions."""
import os

import boto3
import pika
from google.cloud import pubsub_v1


def audit_orders(channel):
    channel.queue_bind(queue="inventory-audit", exchange="orders", routing_key="#")
    channel.basic_consume(queue="inventory-audit", on_message_callback=print)


def send_email(channel, body):
    channel.basic_publish(exchange="", routing_key="emails", body=body)


def ship(order_id):
    sqs = boto3.client("sqs")
    sqs.send_message(QueueUrl=os.environ["SHIPMENTS_QUEUE_URL"], MessageBody=order_id)


def reserve_invoiced_stock(project):
    publisher = pubsub_v1.PublisherClient()
    subscriber = pubsub_v1.SubscriberClient()
    topic_path = publisher.topic_path(project, "invoices")
    sub_path = subscriber.subscription_path(project, "invoices-inventory")
    subscriber.create_subscription(request={"name": sub_path, "topic": topic_path})
    subscriber.subscribe(sub_path, callback=print)
