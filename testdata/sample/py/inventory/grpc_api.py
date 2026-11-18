"""The inventory gRPC API, and a client of the orders services."""
import grpc

from . import inventory_pb2_grpc, orders_pb2, orders_pb2_grpc


class InventoryServicer(inventory_pb2_grpc.InventoryServiceServicer):
    """Serves InventoryService."""

    def GetStock(self, request, context):
        return None

    def Reserve(self, request, context):
        return None

    def _audit(self, sku):
        pass


class OrdersClient:
    """Reads orders from checkout over gRPC."""

    def __init__(self, channel):
        self.orders = orders_pb2_grpc.OrderServiceStub(channel)

    def order(self, order_id):
        return self.orders.GetOrder(orders_pb2.GetOrderRequest(id=order_id))


def refund(order_id):
    with grpc.insecure_channel("orders:9090") as channel:
        stub = orders_pb2_grpc.RefundServiceStub(channel)
        return stub.Refund(orders_pb2.RefundRequest(id=order_id))
