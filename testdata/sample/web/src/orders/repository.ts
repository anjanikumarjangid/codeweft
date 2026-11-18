/** Storage of orders. */
export interface OrderRepository {
  find(id: string): Promise<Order | undefined>;
  save(order: Order): Promise<void>;
}

export interface Order {
  id: string;
  cents: number;
}

/** Keeps orders in memory. */
export class MemoryRepository implements OrderRepository {
  private orders = new Map<string, Order>();

  async find(id: string): Promise<Order | undefined> {
    return this.orders.get(id);
  }

  async save(order: Order): Promise<void> {
    this.orders.set(order.id, order);
  }
}

/** A repository that counts what it saves. */
export class CountingRepository extends MemoryRepository {
  saved = 0;

  async save(order: Order): Promise<void> {
    this.saved++;
    await super.save(order);
  }
}
