package grvt

import (
	"context"
	"iter"
)

// BackfillPage is available REST history, not proof of complete event recovery.
type BackfillPage[P Page] struct {
	Page             P
	Gap              GapRange
	HistoryExhausted bool
}

func (p BackfillPage[P]) Continuity() string { return "unproven" }

// BackfillPages preserves the original gap while reading available history.
// fetch owns endpoint selection and the time window. Sequence bounds cannot be
// used as REST cursors, and exhausted history does not prove event visibility.
func BackfillPages[P Page](ctx context.Context, fetch func(context.Context, string) (P, error), gap GapRange) iter.Seq2[BackfillPage[P], error] {
	original := copyGapRange(&gap)
	return func(yield func(BackfillPage[P], error) bool) {
		for page, err := range Pages(ctx, fetch, "") {
			if err != nil {
				yield(BackfillPage[P]{}, err)
				return
			}
			result := BackfillPage[P]{Page: page, Gap: *copyGapRange(original), HistoryExhausted: page.NextCursor() == ""}
			if !yield(result, nil) {
				return
			}
		}
	}
}
