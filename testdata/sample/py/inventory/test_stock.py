from .models import Item, ItemRepository
from .stock import StockService


def make_service():
    return StockService(ItemRepository())


def test_reserve_fails_without_stock():
    service = make_service()
    assert not service.reserve("sku", 1)


class TestItem:
    def test_item_keeps_count(self):
        item = Item("sku", 3)
        assert item.count == 3
