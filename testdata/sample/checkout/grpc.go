package checkout

import (
	"context"

	"google.golang.org/grpc"

	inventoryv1 "example.com/sample/gen/inventoryv1"
	ordersv1 "example.com/sample/gen/ordersv1"
)

// orderServer serves the OrderService gRPC API.
type orderServer struct {
	ordersv1.UnimplementedOrderServiceServer
}

// GetOrder returns one order.
func (s *orderServer) GetOrder(ctx context.Context, req *ordersv1.GetOrderRequest) (*ordersv1.Order, error) {
	return &ordersv1.Order{Id: req.Id}, nil
}

// ListOrders streams a customer's orders.
func (s *orderServer) ListOrders(req *ordersv1.ListOrdersRequest, stream ordersv1.OrderService_ListOrdersServer) error {
	return nil
}

// Close is not an RPC method.
func (s *orderServer) Close() {}

// ServeGRPC registers the gRPC services on a server.
func ServeGRPC(s *grpc.Server) {
	ordersv1.RegisterOrderServiceServer(s, &orderServer{})
}

// stockClient asks the inventory service over gRPC.
type stockClient struct {
	inventory inventoryv1.InventoryServiceClient
}

// available returns the stock of an item.
func available(ctx context.Context, conn *grpc.ClientConn, sku string) (int32, error) {
	c := inventoryv1.NewInventoryServiceClient(conn)
	st, err := c.GetStock(ctx, &inventoryv1.GetStockRequest{Sku: sku})
	if err != nil {
		return 0, err
	}
	return st.Available, nil
}

// hold reserves items through the client's connection.
func (s *stockClient) hold(ctx context.Context, sku string) error {
	_, err := s.inventory.Reserve(ctx, &inventoryv1.ReserveRequest{Sku: sku, Quantity: 1})
	return err
}
