package rawblock

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

const (
	// Page size.
	defaultBatchSize       = 4096
	defaultMaxPendingBytes = 10000000
	defaultFlushEvery      = time.Millisecond

	commitLogKey            = "commitlog-"
	commitLogKeyTupleLength = 2
)

type clStatus int

const (
	clStatusUnopened clStatus = iota
	clStatusOpen
	clStatusClosed
)

// truncationToken is a token that can be passed to the commitlog to truncate the commitlogs up to
// a specific point. It should be treated as opaque by external callers.
type truncationToken struct {
	upTo tuple.Tuple
}

// Commitlog is the interface for an FDB-backed commitlog.
type Commitlog interface {
	Write([]byte) error
	Open() error
	Close() error
	WaitForRotation() (truncationToken, error)
	Truncate(token truncationToken) error
}

// CommitlogOptions encapsulates the options for the commit log.
type CommitlogOptions struct {
	IdealBatchSize  int
	MaxPendingBytes int
	FlushEvery      time.Duration
}

// NewCommitlogOptions creates a new CommitlogOptions.
func NewCommitlogOptions() CommitlogOptions {
	return CommitlogOptions{
		IdealBatchSize:  defaultBatchSize,
		MaxPendingBytes: defaultMaxPendingBytes,
		FlushEvery:      defaultFlushEvery,
	}
}

type flushOutcome struct {
	// TODO(rartoul): Fix this, but last ID can be nil in the case
	// that there was no data to flush. This is useful because it
	// enables the WaitForRotation() API.
	lastID tuple.Tuple
	err    error
	doneCh chan struct{}
}

func newFlushOutcome() *flushOutcome {
	return &flushOutcome{
		doneCh: make(chan struct{}, 0),
	}
}

func (f *flushOutcome) waitForFlush() error {
	<-f.doneCh
	return f.err
}

func (f *flushOutcome) notify(lastID tuple.Tuple, err error) {
	f.lastID = lastID
	f.err = err
	close(f.doneCh)
}

type commitlog struct {
	sync.Mutex
	status        clStatus
	db            fdb.Database
	prevBatch     []byte
	currBatch     []byte
	lastFlushTime time.Time
	lastIdx       int64
	flushOutcome  *flushOutcome
	closeCh       chan struct{}
	closeDoneCh   chan error
	opts          CommitlogOptions
}

// NewCommitlog creates a new commitlog.
func NewCommitlog(db fdb.Database, opts CommitlogOptions) Commitlog {
	return &commitlog{
		status:       clStatusUnopened,
		db:           db,
		flushOutcome: newFlushOutcome(),
		closeCh:      make(chan struct{}, 1),
		closeDoneCh:  make(chan error, 1),
		opts:         opts,
	}
}

func (c *commitlog) Open() error {
	c.Lock()
	defer c.Unlock()
	if c.status != clStatusUnopened {
		return errors.New("commitlog cannot be opened more than once")
	}

	// "Bootstrap" the latest existing index to maintain a monotonically increasing
	// value for the commitlog chunk indices.
	existingIdx, ok, err := c.getLatestExistingIndex()
	if err != nil {
		return err
	}
	if !ok {
		existingIdx = -1
	}
	c.lastIdx = existingIdx
	fmt.Println(c.lastIdx)

	c.status = clStatusOpen

	go func() {
		for {
			i := 0
			select {
			case <-c.closeCh:
				c.closeDoneCh <- c.flush()
				return
			default:
			}
			time.Sleep(time.Millisecond)
			if err := c.flush(); err != nil {
				log.Printf("error flushing commitlog: %v", err)
			}
			i++
		}
	}()

	return nil
}

func (c *commitlog) Close() error {
	c.Lock()
	if c.status != clStatusOpen {
		c.Unlock()
		return errors.New("cannot close commit log that is not open")
	}
	c.status = clStatusClosed
	c.Unlock()

	c.closeCh <- struct{}{}
	return <-c.closeDoneCh
}

// TODO(rartoul): Kind of gross that this just takes a []byte but more
// flexible for now.
func (c *commitlog) Write(b []byte) error {
	if len(b) == 0 {
		return errors.New("commit log can not write empty chunk")
	}

	c.Lock()
	if c.status != clStatusOpen {
		c.Unlock()
		return errors.New("cannot write into commit log that is not open")
	}

	if len(c.currBatch)+len(b) > c.opts.MaxPendingBytes {
		c.Unlock()
		return errors.New("commit log queue is full")
	}

	c.currBatch = append(c.currBatch, b...)
	currFlushOutcome := c.flushOutcome
	c.Unlock()
	return currFlushOutcome.waitForFlush()
}

func (c *commitlog) Truncate(token truncationToken) error {
	if token.upTo == nil {
		// This can occur in the situation where there were no existing commitlogs when
		// the truncationToken was generated by a call to WaitForRotation().
		return nil
	}

	_, err := c.db.Transact(func(tr fdb.Transaction) (interface{}, error) {
		tr.ClearRange(fdb.KeyRange{Begin: tuple.Tuple{commitLogKey}, End: token.upTo})
		return nil, nil
	})

	return err
}

func (c *commitlog) WaitForRotation() (truncationToken, error) {
	c.Lock()
	if c.status != clStatusOpen {
		c.Unlock()
		return truncationToken{}, errors.New("cannot wait for commit log rotation if commit log is not open")
	}
	currFlushOutcome := c.flushOutcome
	c.Unlock()

	if err := currFlushOutcome.waitForFlush(); err != nil {
		return truncationToken{}, err
	}

	return truncationToken{upTo: currFlushOutcome.lastID}, nil
}

func (c *commitlog) flush() error {
	c.Lock()
	currFlushOutcome := c.flushOutcome
	c.flushOutcome = newFlushOutcome()

	if !(time.Since(c.lastFlushTime) >= c.opts.FlushEvery && len(c.currBatch) > 0) {
		c.Unlock()
		// Notify anyways so that the WaitForRotation() API can function.
		var lastKey tuple.Tuple
		if c.lastIdx >= 0 {
			lastKey = commitlogKeyFromIdx(c.lastIdx)
		}
		currFlushOutcome.notify(lastKey, nil)
		return nil
	}

	toWrite := c.currBatch
	c.currBatch, c.prevBatch = c.prevBatch, c.currBatch
	c.currBatch = c.currBatch[:0]
	c.Unlock()

	key, err := c.db.Transact(func(tr fdb.Transaction) (interface{}, error) {
		// TODO(rartoul): Need to be smarter about this because don't want to actually
		// break chunks across writes I.E every call to WriteBatch() should end up
		// in one key so that each key is a complete unit.
		var (
			startIdx = 0
			key      tuple.Tuple
		)
		for startIdx < len(toWrite) {
			key = c.nextKey()
			endIdx := startIdx + c.opts.IdealBatchSize
			if endIdx > len(toWrite) {
				endIdx = len(toWrite)
			}
			tr.Set(key, toWrite[startIdx:endIdx])
			startIdx = endIdx
		}

		return key, nil
	})
	currFlushOutcome.notify(key.(tuple.Tuple), err)
	return err
}

func (c *commitlog) nextKey() tuple.Tuple {
	// TODO(rartoul): This should have some kind of host identifier in it.
	nextKey := commitlogKeyFromIdx(c.lastIdx + 1)
	// Safe to update this optimistically since even if the write ends up failing
	// its ok to have "gaps".
	//
	// Also safe to do this without any locking as this function is always called
	// in a single-threaded manner.
	c.lastIdx++
	return nextKey
}

func (c *commitlog) getLatestExistingIndex() (int64, bool, error) {
	key, err := c.db.Transact(func(tr fdb.Transaction) (interface{}, error) {
		var (
			rangeResult = tr.GetRange(fdb.KeyRange{
				Begin: tuple.Tuple{commitLogKey, 0},
				End:   tuple.Tuple{commitLogKey, math.MaxInt64}}, fdb.RangeOptions{})
			iter = rangeResult.Iterator()
			key  fdb.Key
		)
		for iter.Advance() {
			curr, err := iter.Get()
			if err != nil {
				return nil, err
			}
			key = curr.Key
		}

		if key == nil {
			return nil, nil
		}
		return key, nil
	})

	if err != nil {
		return -1, false, err
	}
	if key == nil {
		return -1, false, nil
	}

	keyTuple, err := tuple.Unpack(key.(fdb.Key))
	if err != nil {
		return -1, false, err
	}

	if len(keyTuple) != commitLogKeyTupleLength {
		return -1, false, fmt.Errorf(
			"malformed commitlog key tuple, expected len: %d, but was: %d, raw: %v",
			commitLogKeyTupleLength, len(keyTuple), key)
	}
	idx, ok := keyTuple[1].(int64)
	if !ok {
		return -1, false, errors.New("malformed commitlog key tuple, expected second value to be of type int64")
	}
	return idx, true, nil
}

func commitlogKeyFromIdx(idx int64) tuple.Tuple {
	return tuple.Tuple{commitLogKey, idx + 1}
}
