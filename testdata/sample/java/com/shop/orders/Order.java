package com.shop.orders;

import java.util.List;

/** A customer order. */
public record Order(String id, String customerId, List<String> items) {

    /** Creates an order with a generated id. */
    public static Order create(String customerId, List<String> items) {
        return new Order(java.util.UUID.randomUUID().toString(), customerId, items);
    }

    /** Total price in cents. */
    public long total() {
        return items.size() * 100L;
    }
}
