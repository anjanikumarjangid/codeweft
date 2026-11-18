package com.shop.orders;

import com.shop.orders.grpc.Order;
import com.shop.orders.grpc.RefundRequest;
import com.shop.orders.grpc.RefundServiceGrpc;
import com.shop.inventory.grpc.InventoryServiceGrpc;
import com.shop.inventory.grpc.GetStockRequest;
import io.grpc.ManagedChannel;
import io.grpc.stub.StreamObserver;
import java.util.concurrent.TimeUnit;

/** Serves refunds over gRPC. */
public class RefundServer extends RefundServiceGrpc.RefundServiceImplBase {
    private final InventoryServiceGrpc.InventoryServiceBlockingStub inventory;

    RefundServer(ManagedChannel channel) {
        this.inventory = InventoryServiceGrpc.newBlockingStub(channel);
    }

    @Override
    public void refund(RefundRequest request, StreamObserver<Order> responseObserver) {
        this.inventory.withDeadlineAfter(1, TimeUnit.SECONDS);
        inventory.getStock(GetStockRequest.newBuilder().setSku(request.getId()).build());
        responseObserver.onCompleted();
    }

    static void restock(ManagedChannel channel, String sku) {
        InventoryServiceGrpc.InventoryServiceBlockingStub stub = InventoryServiceGrpc.newBlockingStub(channel);
        stub.reserve(null);
    }
}
