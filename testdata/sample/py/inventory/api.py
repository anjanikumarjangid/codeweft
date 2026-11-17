"""HTTP API for inventory."""
from fastapi import APIRouter, FastAPI

from .stock import StockService

router = APIRouter(prefix="/inventory")


@router.get("/stock/{sku}")
def get_stock(sku: str) -> dict:
    """Current stock level for a SKU."""
    return {"sku": sku}


@router.post("/stock/{sku}/reserve")
def reserve(sku: str, qty: int, service: StockService) -> dict:
    """Reserve stock for an order."""
    return {"reserved": service.reserve(sku, qty)}


if __name__ == "__main__":
    import uvicorn

    app = FastAPI()
    app.include_router(router)
    uvicorn.run(app)
