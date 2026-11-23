package audit

import "context"

type operationKey struct{}

type Operation struct {
	SessionID string
	TurnID    string
	StepID    string
}

func WithOperation(ctx context.Context, operation Operation) context.Context {
	return context.WithValue(ctx, operationKey{}, operation)
}

func OperationFrom(ctx context.Context) Operation {
	operation, _ := ctx.Value(operationKey{}).(Operation)
	return operation
}
