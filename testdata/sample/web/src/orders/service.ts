import { Order, OrderRepository } from "./repository";
import { checkout } from "../api/client";

/** Places and looks up orders for the web shop. */
export class OrderService {
  constructor(private readonly repo: OrderRepository) {}

  async place(customerId: string, cents: number): Promise<Order> {
    const order = { id: await checkout(customerId, cents), cents };
    await this.repo.save(order);
    return order;
  }

  async lookup(id: string): Promise<Order | undefined> {
    return this.repo.find(id);
  }
}

export const formatCents = (cents: number): string => `$${(cents / 100).toFixed(2)}`;
