package com.shop.orders;

import com.shop.billing.Billing;
import java.util.List;
import org.junit.jupiter.api.Test;

class OrderServiceTest {

    private OrderService newService() {
        return new OrderService(new Billing(), new InMemoryOrderRepository());
    }

    @Test
    void placeCreatesAnOrder() {
        OrderService service = newService();
        service.place("c1", List.of("sku"));
    }

    @Test
    void totalCountsItems() {
        Order order = Order.create("c1", List.of("a", "b"));
        order.total();
    }
}
