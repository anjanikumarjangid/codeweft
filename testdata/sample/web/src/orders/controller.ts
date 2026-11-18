import { Controller, Get, Param } from "@nestjs/common";
import { OrderService } from "./service";

@Controller("admin/orders")
export class AdminController {
  constructor(private readonly orders: OrderService) {}

  @Get(":id")
  async show(@Param("id") id: string) {
    return this.orders.lookup(id);
  }
}
