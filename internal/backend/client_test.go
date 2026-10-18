package backend

import (
	"context"
	"testing"
)

func TestCallOnNilClientReturnsError(t *testing.T) {
	var client *Client
	if _, err := client.Call(context.Background(), "auth.status", nil, nil); err == nil {
		t.Fatal("Call on a nil client should return an error")
	}
}
