"""Customer notifications and restocking, run on Temporal."""
from temporalio import activity, workflow
from temporalio.client import Client


@activity.defn(name="NotifyCustomer")
async def notify_customer(order_id: str) -> None:
    """Emails the customer that their order shipped."""


@workflow.defn
class RestockWorkflow:
    """Restocks an item and bills the supplier order."""

    @workflow.run
    async def run(self, sku: str) -> None:
        await workflow.execute_activity(notify_customer, sku)
        await workflow.execute_child_workflow("bill-order", sku)


async def restock(client: Client, sku: str) -> None:
    await client.execute_workflow(RestockWorkflow.run, sku, id="restock-" + sku, task_queue="inventory")
