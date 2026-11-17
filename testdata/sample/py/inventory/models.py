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


class CachedItemRepository(ItemRepository):
    """Serves items from memory before falling back to storage."""

    def __init__(self):
        self.cache = {}

    def get(self, sku: str) -> Item:
        """Return the cached item, loading it on a miss."""
        if sku not in self.cache:
            self.cache[sku] = super().get(sku)
        return self.cache[sku]
