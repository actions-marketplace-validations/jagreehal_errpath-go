// Package checkout is the place_order railway example for errpath.
package checkout

// Cart is a shopping cart.
type Cart struct {
	HasCoupon bool
}

// Card is a payment card.
type Card struct{}

// Receipt is an order receipt.
type Receipt struct{}

// PriceError is returned when pricing fails.
type PriceError struct{}

func (e *PriceError) Error() string { return "price" }

// CouponError is returned when a coupon is invalid.
type CouponError struct{}

func (e *CouponError) Error() string { return "coupon" }

// CardDeclined is returned when a charge fails.
type CardDeclined struct{}

func (e *CardDeclined) Error() string { return "card declined" }

// DbError is returned when persistence fails.
type DbError struct{}

func (e *DbError) Error() string { return "db" }

// EmailError is returned when confirmation email fails.
type EmailError struct{}

func (e *EmailError) Error() string { return "email" }

// errpath: error PriceError
func Price(cart Cart) (int, error) {
	_ = cart
	return 10, nil
}

// errpath: error CouponError
func ValidateCoupon(cart Cart) error {
	_ = cart
	return nil
}

// errpath: error CardDeclined
func Charge(card Card, total int) (Receipt, error) {
	_, _ = card, total
	return Receipt{}, nil
}

// errpath: error DbError
func SaveOrder(cart Cart, payment Receipt) (Receipt, error) {
	_, _ = cart, payment
	return payment, nil
}

// errpath: error EmailError
func SendConfirmation(receipt Receipt) error {
	_ = receipt
	return nil
}

// PlaceOrder runs the checkout railway.
func PlaceOrder(cart Cart, card Card) (Receipt, error) {
	total, err := Price(cart)
	if err != nil {
		return Receipt{}, err
	}
	if cart.HasCoupon {
		if err := ValidateCoupon(cart); err != nil {
			return Receipt{}, err
		}
	}
	payment, err := Charge(card, total)
	if err != nil {
		return Receipt{}, err
	}
	receipt, err := SaveOrder(cart, payment)
	if err != nil {
		return Receipt{}, err
	}
	if err := SendConfirmation(receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
