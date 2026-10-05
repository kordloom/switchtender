package run

import "fmt"

// MaxQueueBytes bounds a queue name. A run's queue is stored under the index pending work is
// claimed through, and PostgreSQL refuses an index entry past about 2.7 kilobytes, so a longer name
// that did not compress failed the submit with a 500 rather than a refusal. A queue names a group
// of workers, which takes a word or two.
const MaxQueueBytes = 255

// CheckQueue returns ErrQueueTooLong, stating the limit, for a queue name longer than
// MaxQueueBytes.
func CheckQueue(queue string) error {
	if len(queue) > MaxQueueBytes {
		return fmt.Errorf("%w: a queue name may be at most %d bytes, and this one is %d",
			ErrQueueTooLong, MaxQueueBytes, len(queue))
	}
	return nil
}
