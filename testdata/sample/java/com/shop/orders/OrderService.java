package com.shop.orders;

import com.shop.billing.Billing;
import java.util.List;

/**
 * Places and cancels orders.
 */
public class OrderService {
    private final Billing billing;
    private final OrderRepository repo;

    public OrderService(Billing billing, OrderRepository repo) {
        this.billing = billing;
        this.repo = repo;
    }

    /** Places an order and charges the customer. */
    public Order place(String customerId, List<String> items) {
        Order order = Order.create(customerId, items);
        billing.charge(customerId, order.total());
        repo.save(order);
        audit(order);
        return order;
    }

    private void audit(Order order) {
        System.out.println("placed " + order.id());
    }
}
