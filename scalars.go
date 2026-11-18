package grvt

import (
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

// DecimalText is used by generated signing code; an absent value stays absent.
func DecimalText(value *models.Decimal) string {
	if value == nil {
		return ""
	}
	return value.String()
}
