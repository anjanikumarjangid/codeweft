import express from "express";
import { OrderService, formatCents } from "./orders/service";
import { MemoryRepository } from "./orders/repository";

const app = express();
const router = express.Router();
const service = new OrderService(new MemoryRepository());

async function getOrder(req: any, res: any) {
  const order = await service.lookup(req.params.id);
  res.json({ ...order, total: order ? formatCents(order.cents) : undefined });
}

router.get("/orders/:id", getOrder);
router.post("/orders", async (req: any, res: any) => res.json(await service.place(req.body.customer, req.body.cents)));
app.use("/web", router);
app.listen(3000);
