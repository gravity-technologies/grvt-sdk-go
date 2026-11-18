package grvt

import (
	"context"
	"fmt"
	"iter"
)

// Page exposes the continuation cursor returned by a paginated endpoint.
// Generated response models implement this interface.
type Page interface{ NextCursor() string }

// PaginationCycleError stops a traversal before requesting the same cursor twice.
type PaginationCycleError struct{ Cursor string }

func (e *PaginationCycleError) Error() string {
	return fmt.Sprintf("pagination cursor repeated: %q", e.Cursor)
}

// Pages fetches complete pages lazily, including empty pages with a next cursor.
// Breaking iteration sends no more calls. fetch owns the filters and timeout;
// Each range starts at cursor. ctx cancels the traversal. Cursors do not guarantee a stable server snapshot.
func Pages[P Page](ctx context.Context, fetch func(context.Context, string) (P, error), cursor string) iter.Seq2[P, error] {
	return func(yield func(P, error) bool) {
		current := cursor
		seen := map[string]bool{}
		var zero P
		for {
			if err := ctx.Err(); err != nil {
				yield(zero, err)
				return
			}
			if current != "" {
				if seen[current] {
					yield(zero, &PaginationCycleError{Cursor: current})
					return
				}
				seen[current] = true
			}
			page, err := fetch(ctx, current)
			if err != nil {
				yield(zero, err)
				return
			}
			if !yield(page, nil) {
				return
			}
			current = page.NextCursor()
			if current == "" {
				return
			}
		}
	}
}
