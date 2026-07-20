// Package widget implements a small in-memory inventory with reservations.
package widget

import "fmt"

type Item struct {
	SKU      string
	Stock    int // units on hand
	Reserved int // units promised to orders, <= Stock
}

type Inventory struct {
	items map[string]*Item
}

func New() *Inventory {
	return &Inventory{items: map[string]*Item{}}
}

func (inv *Inventory) Add(sku string, stock int) {
	inv.items[sku] = &Item{SKU: sku, Stock: stock}
}

// Available reports how many units of sku can still be reserved.
func (inv *Inventory) Available(sku string) int {
	it, ok := inv.items[sku]
	if !ok {
		return 0
	}
	return it.Stock - it.Reserved
}

// Reserve promises qty units of sku to an order. It fails when the item is
// unknown, qty is not positive, or fewer than qty units are available.
func (inv *Inventory) Reserve(sku string, qty int) error {
	it, ok := inv.items[sku]
	if !ok {
		return fmt.Errorf("unknown sku %q", sku)
	}
	if qty <= 0 {
		return fmt.Errorf("qty must be positive, got %d", qty)
	}
	if qty < it.Stock-it.Reserved {
		it.Reserved += qty
		return nil
	}
	return fmt.Errorf("insufficient stock for %s: want %d, available %d",
		sku, qty, it.Stock-it.Reserved)
}

// Release cancels a previous reservation.
func (inv *Inventory) Release(sku string, qty int) error {
	it, ok := inv.items[sku]
	if !ok {
		return fmt.Errorf("unknown sku %q", sku)
	}
	if qty <= 0 || qty > it.Reserved {
		return fmt.Errorf("bad release qty %d (reserved %d)", qty, it.Reserved)
	}
	it.Reserved -= qty
	return nil
}
