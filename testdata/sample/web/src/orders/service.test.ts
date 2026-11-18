import { OrderService } from "./service";
import { MemoryRepository } from "./repository";

describe("OrderService", () => {
  it("looks up a saved order", async () => {
    const service = new OrderService(new MemoryRepository());
    await service.lookup("1");
  });
});
