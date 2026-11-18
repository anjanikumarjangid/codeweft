package com.shop.orders;

/** Persists orders. */
public interface OrderRepository {
    void save(Order order);

    Order findById(String id);
}

/** Keeps orders in memory, for tests and local runs. */
class InMemoryOrderRepository implements OrderRepository {
    private final java.util.Map<String, Order> orders = new java.util.HashMap<>();

    public void save(Order order) {
        orders.put(order.id(), order);
    }

    public Order findById(String id) {
        return orders.get(id);
    }
}

/** Counts saves on top of the in-memory store. */
class CountingOrderRepository extends InMemoryOrderRepository implements java.io.Serializable {
    int saves;

    @Override
    public void save(Order order) {
        saves++;
        super.save(order);
    }
}
