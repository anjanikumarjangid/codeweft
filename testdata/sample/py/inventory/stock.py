"""Stock levels for the warehouse."""
import logging

from .models import Item, ItemRepository


class StockService:
    """Tracks and reserves stock per item."""

    def __init__(self, repo: ItemRepository):
        self.repo = repo

    def reserve(self, sku: str, qty: int) -> bool:
        """Reserve qty units of sku, if enough are in stock."""
        item = self.repo.get(sku)
        if not _enough(item, qty):
            return False
        self._log(sku, qty)
        return True

    def _log(self, sku, qty):
        logging.info("reserved %s x%d", sku, qty)


def _enough(item: Item, qty: int) -> bool:
    return item.count >= qty
