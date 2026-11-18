package com.shop.orders;

import java.util.List;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

/** HTTP API for orders. */
@RestController
@RequestMapping("/orders")
public class OrderController {
    private final OrderService orders;

    public OrderController(OrderService orders) { this.orders = orders; }

    @PostMapping
    public Order place(@RequestBody PlaceRequest req) {
        return orders.place(req.customerId(), req.items());
    }

    /** Looks up one order. */
    @GetMapping("/{id}")
    public Order get(@PathVariable String id) {
        return null;
    }

    /** Request body for placing an order. */
    public record PlaceRequest(String customerId, List<String> items) {}
}

/** The checkout service, called over HTTP. */
@FeignClient(name = "checkout", path = "/api/v1")
interface CheckoutClient {
    @PostMapping("/checkout")
    String checkout(String cart);
}

/** Looks orders up in the checkout service. */
class CheckoutOrders {
    static final String ORDERS = "/api/v1/orders/";
    private final org.springframework.web.client.RestTemplate rest = new org.springframework.web.client.RestTemplate();

    String find(String id) {
        return rest.getForObject("http://checkout" + ORDERS + id, String.class);
    }
}
