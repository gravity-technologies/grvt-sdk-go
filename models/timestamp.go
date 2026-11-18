package models

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// wireTimestamp only controls JSON encoding. Public model fields use time.Time.
type wireTimestamp time.Time

func (t wireTimestamp) MarshalJSON() ([]byte, error) {
	value := time.Time(t)
	ns := value.UnixNano()
	if !time.Unix(0, ns).Equal(value) {
		return nil, fmt.Errorf("timestamp is outside the signed 64-bit nanosecond range")
	}
	return json.Marshal(strconv.FormatInt(ns, 10))
}

func (t *wireTimestamp) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	ns, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return err
	}
	*t = wireTimestamp(time.Unix(0, ns).UTC())
	return nil
}
