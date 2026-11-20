package canarynode

import "context"

type condorWindowReaderContextKey struct{}

func withCondorWindowReader(ctx context.Context, reader condorWindowReader) context.Context {
	return context.WithValue(ctx, condorWindowReaderContextKey{}, reader)
}

func condorWindowReaderFromContext(ctx context.Context) condorWindowReader {
	reader, _ := ctx.Value(condorWindowReaderContextKey{}).(condorWindowReader)
	return reader
}
