"""Inventory data model."""


class Item:
    """A stock-keeping unit and its count."""

    def __init__(self, sku: str, count: int):
        self.sku = sku
        self.count = count


class ItemRepository:
    """Loads items from storage."""

    def get(self, sku: str) -> Item:
        """Return the item for a SKU."""
        return Item(sku, 0)
