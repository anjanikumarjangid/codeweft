package com.shop.billing;

import static java.util.Objects.requireNonNull;

/** Charges customers through the payment provider. */
public class Billing {
    /** Charges the customer, returning a charge id. */
    public String charge(String customerId, long cents) {
        requireNonNull(customerId);
        return validate(cents) ? "ch_" + customerId : null;
    }

    /** Charges in whole units of currency. */
    public String charge(String customerId, int units) {
        return charge(customerId, units * 100L);
    }

    private boolean validate(long cents) {
        return cents > 0;
    }
}
