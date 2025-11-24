package ironhawk

import (
	"context"
	"errors"
	"strings"
)

type ctxKey int

const (
	fireClientKey ctxKey = iota + 1
)

// WithFireClient injects a fireClient into context for ackEmission/reconnect helpers.
func WithFireClient(ctx context.Context, c fireClient) context.Context {
	return context.WithValue(ctx, fireClientKey, c)
}

// ackEmission sends EMITACK for the given key/subscription/index using client from context.
func ackEmission(ctx context.Context, key, subID string, idx uint64) error {
	v := ctx.Value(fireClientKey)
	fc, ok := v.(fireClient)
	if !ok || fc == nil {
		return errors.New("no fire client in context")
	}
	return sendEmitAck(fc, key, subID, idx)
}

// reconnect calls EMITRECONNECT and returns the next index on OK.
func reconnect(ctx context.Context, key, subID string, lastIdx uint64) (uint64, error) {
	v := ctx.Value(fireClientKey)
	fc, ok := v.(fireClient)
	if !ok || fc == nil {
		return 0, errors.New("no fire client in context")
	}
	status, next, err := emitReconnect(fc, key, subID, lastIdx)
	if err != nil {
		return 0, err
	}
	if strings.ToUpper(status) != "OK" {
		return 0, nil
	}
	return next, nil
}
