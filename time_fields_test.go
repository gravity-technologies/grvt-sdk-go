package grvt

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/models"
)

func TestHistoryFilterNanoseconds(t *testing.T) {
	value := time.Unix(0, 1793172225200080949).UTC()
	request := models.ApiFillHistoryRequest{SubAccountID: 123, StartTime: &value}
	wire, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"start_time":"1793172225200080949"`) {
		t.Fatalf("lost precision: %s", wire)
	}
	var decoded models.ApiFillHistoryRequest
	if err = json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.StartTime == nil || !decoded.StartTime.Equal(value) {
		t.Fatal("lost timestamp")
	}
}
