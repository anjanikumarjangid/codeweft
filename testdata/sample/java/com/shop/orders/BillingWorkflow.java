package com.shop.orders;

import io.temporal.activity.ActivityInterface;
import io.temporal.activity.ActivityMethod;
import io.temporal.activity.ActivityOptions;
import io.temporal.client.WorkflowClient;
import io.temporal.client.WorkflowOptions;
import io.temporal.worker.Worker;
import io.temporal.worker.WorkerFactory;
import io.temporal.workflow.SignalMethod;
import io.temporal.workflow.Workflow;
import io.temporal.workflow.WorkflowInterface;
import io.temporal.workflow.WorkflowMethod;

/** Bills an order: a Temporal workflow. */
@WorkflowInterface
public interface BillingWorkflow {
    @WorkflowMethod(name = "bill-order")
    void bill(String orderId);

    @SignalMethod
    void cancel();
}

@ActivityInterface
interface BillingActivities {
    void chargeCard(String orderId);

    @ActivityMethod(name = "refund")
    void refundCard(String orderId);
}

class BillingWorkflowImpl implements BillingWorkflow {
    private final BillingActivities activities =
        Workflow.newActivityStub(BillingActivities.class, ActivityOptions.newBuilder().build());

    public void bill(String orderId) {
        activities.chargeCard(orderId);
        Workflow.newUntypedChildWorkflowStub("ShipOrder").execute(Void.class, orderId);
    }

    public void cancel() {}
}

class BillingActivitiesImpl implements BillingActivities {
    public void chargeCard(String orderId) {}

    public void refundCard(String orderId) {}
}

class BillingStarter {
    void start(WorkflowClient client, String orderId) {
        BillingWorkflow wf = client.newWorkflowStub(BillingWorkflow.class,
            WorkflowOptions.newBuilder().setTaskQueue("billing").build());
        wf.bill(orderId);
    }

    void serve(WorkerFactory factory) {
        Worker worker = factory.newWorker("billing");
        worker.registerWorkflowImplementationTypes(BillingWorkflowImpl.class);
        worker.registerActivitiesImplementations(new BillingActivitiesImpl());
    }
}
