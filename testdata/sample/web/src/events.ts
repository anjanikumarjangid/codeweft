import { Kafka } from "kafkajs";

const kafka = new Kafka({ clientId: "web", brokers: ["kafka:9092"] });

/** Announces orders placed on the web shop. */
export async function announce(id: string) {
  const producer = kafka.producer();
  await producer.send({ topic: "orders.placed", messages: [{ value: id }] });
}

/** Shows stock changes to shoppers. */
export async function followStock() {
  const consumer = kafka.consumer({ groupId: "web" });
  await consumer.subscribe({ topic: "stock.changed" });
}
