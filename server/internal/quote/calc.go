package quote

import (
	"fmt"
	"math"
	"strings"
)

type ItemInput struct {
	Kind           string
	Description    string
	Quantity       int
	UnitPriceCents int64
}

// Calculate derives per-item amounts and the subtotal from server-side values.
// Clients never send amount_cents; quantity and unit price are the only inputs.
func Calculate(items []ItemInput, discountCents, taxCents int64) (int64, int64, error) {
	if len(items) == 0 {
		return 0, 0, fmt.Errorf("%w: at least one item is required", ErrInvalidInput)
	}
	if len(items) > 100 {
		return 0, 0, fmt.Errorf("%w: at most 100 items", ErrInvalidInput)
	}
	if discountCents < 0 || taxCents < 0 {
		return 0, 0, fmt.Errorf("%w: discount and tax must not be negative", ErrInvalidInput)
	}
	var subtotal int64
	for i := range items {
		if err := validateItem(&items[i]); err != nil {
			return 0, 0, err
		}
		amount := items[i].UnitPriceCents * int64(items[i].Quantity)
		if items[i].UnitPriceCents != 0 && amount/items[i].UnitPriceCents != int64(items[i].Quantity) {
			return 0, 0, fmt.Errorf("%w: item %d overflows", ErrInvalidInput, i+1)
		}
		if subtotal > math.MaxInt64-amount {
			return 0, 0, fmt.Errorf("%w: subtotal overflows", ErrInvalidInput)
		}
		subtotal += amount
	}
	if discountCents > math.MaxInt64-taxCents {
		return 0, 0, fmt.Errorf("%w: total overflows", ErrInvalidInput)
	}
	total := subtotal - discountCents + taxCents
	if total < 0 {
		return 0, 0, fmt.Errorf("%w: discount exceeds subtotal", ErrInvalidInput)
	}
	return subtotal, total, nil
}

func validateItem(item *ItemInput) error {
	item.Kind = strings.TrimSpace(item.Kind)
	item.Description = strings.TrimSpace(item.Description)
	if item.Kind == "" {
		item.Kind = "service"
	}
	switch item.Kind {
	case "service", "retouch", "production", "travel", "rush", "license", "other":
	default:
		return fmt.Errorf("%w: unknown item kind %q", ErrInvalidInput, item.Kind)
	}
	if item.Description == "" {
		return fmt.Errorf("%w: item description is required", ErrInvalidInput)
	}
	if item.Quantity <= 0 || item.Quantity > 1_000_000 {
		return fmt.Errorf("%w: item quantity must be between 1 and 1000000", ErrInvalidInput)
	}
	if item.UnitPriceCents < 0 {
		return fmt.Errorf("%w: item unit price must not be negative", ErrInvalidInput)
	}
	return nil
}
