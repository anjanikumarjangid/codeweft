import axios from "axios";

const CHECKOUT = process.env.CHECKOUT_URL;

/** Pays a cart with the checkout service (Go). */
export async function checkout(customerId: string, cents: number): Promise<string> {
  const res = await fetch(`${CHECKOUT}/api/v1/checkout`, { method: "POST", body: JSON.stringify({ customerId, cents }) });
  return (await res.json()).id;
}

/** Reads an order from the orders service (Java). */
export async function javaOrder(id: string) {
  return axios.get(`http://orders:8080/orders/${id}`);
}
