package kgo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// TransactionEndTry is simply a named bool.
type TransactionEndTry bool

const (
	// TryAbort attempts to end a transaction with an abort.
	TryAbort TransactionEndTry = false

	// TryCommit attempts to end a transaction with a commit.
	TryCommit TransactionEndTry = true
)

// GroupTransactSession abstracts away the proper way to begin a transaction
// and more importantly how to end a transaction when consuming in a group,
// modifying records, and producing (EOS transaction).
//
// If you are running Kafka 2.5+, it is strongly recommended that you also use
// RequireStableFetchOffsets. See that config option's documentation for more
// details.
type GroupTransactSession struct {
	cl *Client

	cooperative bool

	failMu sync.Mutex

	revoked   bool
	revokedCh chan struct{} // closed once when revoked is set; reset after End
	lost      bool
	lostCh    chan struct{} // closed once when lost is set; reset after End
}

// NewGroupTransactSession is exactly the same as NewClient, but wraps the
// client's OnRevoked / OnLost to ensure that transactions are correctly
// aborted whenever necessary so as to properly provide EOS.
//
// When ETLing in a group in a transaction, if a rebalance happens before the
// transaction is ended, you either (a) must block the rebalance from finishing
// until you are done producing, and then commit before unblocking, or (b)
// allow the rebalance to happen, but abort any work you did.
//
// The problem with (a) is that if your ETL work loop is slow, you run the risk
// of exceeding the rebalance timeout and being kicked from the group. You will
// try to commit, and depending on the Kafka version, the commit may even be
// erroneously successful (pre Kafka 2.5.0). This will lead to duplicates.
//
// Instead, for safety, a GroupTransactSession favors (b). If a rebalance
// occurs at any time before ending a transaction with a commit, this will
// abort the transaction.
//
// This leaves the risk that ending the transaction itself exceeds the
// rebalance timeout, but this is just one request with no cpu logic. With a
// proper rebalance timeout, this single request will not fail and the commit
// will succeed properly.
//
// If this client detects you are talking to a pre-2.5 cluster, OR if you have
// not enabled RequireStableFetchOffsets, the client will sleep for 200ms after
// a successful commit to allow Kafka's txn markers to propagate. This is not
// foolproof in the event of some extremely unlikely communication patterns and
// **potentially** could allow duplicates. See this repo's transaction's doc
// for more details.
func NewGroupTransactSession(opts ...Opt) (*GroupTransactSession, error) {
	s := &GroupTransactSession{
		revokedCh: make(chan struct{}),
		lostCh:    make(chan struct{}),
	}

	var noGroup error

	// We append one option, which will get applied last.  Because it is
	// applied last, we can execute some logic and override some existing
	// options.
	opts = append(opts, groupOpt{func(cfg *cfg) {
		if cfg.group == "" {
			cfg.seedBrokers = nil // force a validation error
			noGroup = errors.New("missing required group")
			return
		}

		s.cooperative = cfg.cooperative()

		userRevoked := cfg.onRevoked
		cfg.onRevoked = func(ctx context.Context, cl *Client, rev map[string][]int32) {
			s.failMu.Lock()
			defer s.failMu.Unlock()
			if s.revoked {
				return
			}

			if s.cooperative && len(rev) == 0 && !s.revoked {
				cl.cfg.logger.Log(LogLevelInfo, "transact session in on_revoke with nothing to revoke; allowing next commit")
			} else {
				cl.cfg.logger.Log(LogLevelInfo, "transact session in on_revoke; aborting next commit if we are currently in a transaction")
				s.revoked = true
				close(s.revokedCh)
			}

			if userRevoked != nil {
				userRevoked(ctx, cl, rev)
			}
		}

		userLost := cfg.onLost
		cfg.onLost = func(ctx context.Context, cl *Client, lost map[string][]int32) {
			s.failMu.Lock()
			defer s.failMu.Unlock()
			if s.lost {
				return
			}

			cl.cfg.logger.Log(LogLevelInfo, "transact session in on_lost; aborting next commit if we are currently in a transaction")
			s.lost = true
			close(s.lostCh)

			if userLost != nil {
				userLost(ctx, cl, lost)
			} else if userRevoked != nil {
				userRevoked(ctx, cl, lost)
			}
		}
	}})

	cl, err := NewClient(opts...)
	if err != nil {
		if noGroup != nil {
			err = noGroup
		}
		return nil, err
	}
	s.cl = cl
	return s, nil
}

// Client returns the underlying client that this transact session wraps.  This
// can be useful for functions that require a client, such as raw requests. The
// returned client should not be used to manage transactions (leave that to the
// GroupTransactSession).
func (s *GroupTransactSession) Client() *Client {
	return s.cl
}

// Close is a wrapper around Client.Close, with the exact same semantics.
// Refer to that function's documentation.
//
// This function must be called to leave the group before shutting down.
func (s *GroupTransactSession) Close() {
	s.cl.Close()
}

// PollFetches is a wrapper around Client.PollFetches, with the exact same
// semantics. Refer to that function's documentation.
//
// It is invalid to call PollFetches concurrently with Begin or End.
func (s *GroupTransactSession) PollFetches(ctx context.Context) Fetches {
	return s.cl.PollFetches(ctx)
}

// PollRecords is a wrapper around Client.PollRecords, with the exact same
// semantics. Refer to that function's documentation.
//
// It is invalid to call PollRecords concurrently with Begin or End.
func (s *GroupTransactSession) PollRecords(ctx context.Context, maxPollRecords int) Fetches {
	return s.cl.PollRecords(ctx, maxPollRecords)
}

// ProduceSync is a wrapper around Client.ProduceSync, with the exact same
// semantics. Refer to that function's documentation.
//
// It is invalid to call ProduceSync concurrently with Begin or End.
func (s *GroupTransactSession) ProduceSync(ctx context.Context, rs ...*Record) ProduceResults {
	return s.cl.ProduceSync(ctx, rs...)
}

// Produce is a wrapper around Client.Produce, with the exact same semantics.
// Refer to that function's documentation.
//
// It is invalid to call Produce concurrently with Begin or End.
func (s *GroupTransactSession) Produce(ctx context.Context, r *Record, promise func(*Record, error)) {
	s.cl.Produce(ctx, r, promise)
}

// TryProduce is a wrapper around Client.TryProduce, with the exact same
// semantics. Refer to that function's documentation.
//
// It is invalid to call TryProduce concurrently with Begin or End.
func (s *GroupTransactSession) TryProduce(ctx context.Context, r *Record, promise func(*Record, error)) {
	s.cl.TryProduce(ctx, r, promise)
}

// Begin begins a transaction, returning an error if the client has no
// transactional id or is already in a transaction.
//
// Begin must be called before producing records in a transaction.
//
// Note that a revoke of any partitions sets the session's revoked state, even
// if the session has not begun. This state is only reset on EndTransaction.
// Thus, it is safe to begin transactions after a poll (but still before you
// produce).
func (s *GroupTransactSession) Begin() error {
	s.cl.cfg.logger.Log(LogLevelInfo, "beginning transact session")
	return s.cl.BeginTransaction()
}

func (s *GroupTransactSession) failed() bool {
	return s.revoked || s.lost
}

// End ends a transaction, committing if commit is true, if the group did not
// rebalance since the transaction began, and if committing offsets is
// successful. If commit is false, the group has rebalanced, or any partition
// in committing offsets fails, this aborts.
//
// This function calls Flush or AbortBufferedRecords depending on the commit
// status. If you are flushing, it is strongly recommended to Flush yourself
// before calling this, so that you can then determine if you need to abort.
//
// This returns whether the transaction committed or any error that occurred.
// No returned error is retriable. Either the transactional ID has entered a
// failed state, or the client retried so much that the retry limit was hit,
// and odds are you should not continue.
//
// Note that canceling the context will likely leave the client in an
// undesirable state, because canceling the context cancels in flight requests
// and prevents new requests (multiple requests are issued at the end of a
// transact session). Thus, while a context is allowed, it is strongly
// recommended to not cancel it.
func (s *GroupTransactSession) End(ctx context.Context, commit TransactionEndTry) (committed bool, err error) {
	defer func() {
		s.failMu.Lock()
		s.revoked = false
		s.revokedCh = make(chan struct{})
		s.lost = false
		s.lostCh = make(chan struct{})
		s.failMu.Unlock()
	}()

	switch commit {
	case TryCommit:
		if err := s.cl.Flush(ctx); err != nil {
			return false, err // we do not abort below, because an error here is ctx closing
		}
	case TryAbort:
		if err := s.cl.AbortBufferedRecords(ctx); err != nil {
			return false, err // same
		}
	}

	wantCommit := bool(commit)

	s.failMu.Lock()
	failed := s.failed()

	precommit := s.cl.CommittedOffsets()
	postcommit := s.cl.UncommittedOffsets()
	s.failMu.Unlock()

	var hasAbortableCommitErr bool
	var commitErr error
	var g *groupConsumer

	kip447 := false
	if wantCommit && !failed {
		var commitErrs []string

		committed := make(chan struct{})
		g = s.cl.commitTransactionOffsets(context.Background(), postcommit,
			func(_ *kmsg.TxnOffsetCommitRequest, resp *kmsg.TxnOffsetCommitResponse, err error) {
				defer close(committed)
				if err != nil {
					commitErrs = append(commitErrs, err.Error())
					return
				}
				kip447 = resp.Version >= 3

				for _, t := range resp.Topics {
					for _, p := range t.Partitions {
						switch err := kerr.ErrorForCode(p.ErrorCode); err {
						case nil:
						case kerr.IllegalGeneration, // rebalance begun & completed before we committed
							kerr.RebalanceInProgress,     // in rebalance, abort & retry later
							kerr.CoordinatorNotAvailable, // req failed too many times (same for next two)
							kerr.CoordinatorLoadInProgress,
							kerr.NotCoordinator:
							hasAbortableCommitErr = true
						default:
							commitErrs = append(commitErrs, fmt.Sprintf("topic %s partition %d: %v", t.Topic, p.Partition, err))
						}
					}
				}
			},
		)
		<-committed

		if len(commitErrs) > 0 {
			commitErr = fmt.Errorf("unable to commit transaction offsets: %s", strings.Join(commitErrs, ", "))
		}
	}

	// Now that we have committed our offsets, before we allow them to be
	// used, we force a heartbeat. By forcing a heartbeat, if there is no
	// error, then we know we have up to RebalanceTimeout to write our
	// EndTxnRequest without a problem.
	//
	// We should not be booted from the group if we receive an ok
	// heartbeat, meaning that, as mentioned, we should be able to end the
	// transaction safely.
	var okHeartbeat bool
	if g != nil && commitErr == nil {
		waitHeartbeat := make(chan struct{})
		var heartbeatErr error
		select {
		case g.heartbeatForceCh <- func(err error) {
			defer close(waitHeartbeat)
			heartbeatErr = err
		}:
			select {
			case <-waitHeartbeat:
				okHeartbeat = heartbeatErr == nil
			case <-s.revokedCh:
			case <-s.lostCh:
			}
		case <-s.revokedCh:
		case <-s.lostCh:
		}
	}

	s.failMu.Lock()

	// If we know we are KIP-447 and the user is requiring stable, we can
	// unlock immediately because Kafka will itself block a rebalance
	// fetching offsets from outstanding transactions.
	//
	// If either of these are false, we spin up a goroutine that sleeps for
	// 200ms before unlocking to give Kafka a chance to avoid some odd race
	// that would permit duplicates (i.e., what KIP-447 is preventing).
	//
	// This 200ms is not perfect but it should be well enough time on a
	// stable cluster. On an unstable cluster, I still expect clients to be
	// slower than intra-cluster communication, but there is a risk.
	if kip447 && s.cl.cfg.requireStable {
		defer s.failMu.Unlock()
	} else {
		defer func() {
			if committed {
				s.cl.cfg.logger.Log(LogLevelDebug, "sleeping 200ms before allowing a rebalance to continue to give Kafka a chance to write txn markers and avoid duplicates")
				go func() {
					time.Sleep(200 * time.Millisecond)
					s.failMu.Unlock()
				}()
			} else {
				s.failMu.Unlock()
			}
		}()
	}

	tryCommit := !s.failed() && commitErr == nil && !hasAbortableCommitErr && okHeartbeat
	willTryCommit := wantCommit && tryCommit

	s.cl.cfg.logger.Log(LogLevelInfo, "transaction session ending",
		"was_failed", s.failed(),
		"want_commit", wantCommit,
		"can_try_commit", tryCommit,
		"will_try_commit", willTryCommit,
	)

	retried := false // just in case, we use this to avoid looping
retryUnattempted:
	endTxnErr := s.cl.EndTransaction(ctx, TransactionEndTry(willTryCommit))
	if errors.Is(endTxnErr, kerr.OperationNotAttempted) && !retried {
		willTryCommit = false
		retried = true
		s.cl.cfg.logger.Log(LogLevelInfo, "end transaction with commit not attempted; retrying as abort")
		goto retryUnattempted
	}

	if !willTryCommit || endTxnErr != nil {
		currentCommit := s.cl.CommittedOffsets()
		s.cl.cfg.logger.Log(LogLevelInfo, "transact session resetting to current committed state (potentially after a rejoin)",
			"tried_commit", willTryCommit,
			"commit_err", endTxnErr,
			"state_precommit", precommit,
			"state_currently_committed", currentCommit,
		)
		s.cl.setOffsets(currentCommit, false)
	} else if willTryCommit && endTxnErr == nil {
		s.cl.cfg.logger.Log(LogLevelInfo, "transact session successful, setting to newly committed state",
			"tried_commit", willTryCommit,
			"postcommit", postcommit,
		)
		s.cl.setOffsets(postcommit, false)
	}

	switch {
	case commitErr != nil && endTxnErr == nil:
		return false, commitErr

	case commitErr == nil && endTxnErr != nil:
		return false, endTxnErr

	case commitErr != nil && endTxnErr != nil:
		return false, endTxnErr

	default: // both errs nil
		committed = willTryCommit
		return willTryCommit, nil
	}
}

// BeginTransaction sets the client to a transactional state, erroring if there
// is no transactional ID, or if the producer is currently in a fatal
// (unrecoverable) state, or if the client is already in a transaction.
//
// This must not be called concurrently with other client functions.
func (cl *Client) BeginTransaction() error {
	if cl.cfg.txnID == nil {
		return errNotTransactional
	}

	cl.producer.txnMu.Lock()
	defer cl.producer.txnMu.Unlock()

	if cl.producer.inTxn {
		return errors.New("invalid attempt to begin a transaction while already in a transaction")
	}

	needRecover, didRecover, err := cl.maybeRecoverProducerID()
	if needRecover && !didRecover {
		cl.cfg.logger.Log(LogLevelInfo, "unable to begin transaction due to unrecoverable producer id error", "err", err)
		return fmt.Errorf("producer ID has a fatal, unrecoverable error, err: %v", err)
	}

	cl.producer.inTxn = true
	atomic.StoreUint32(&cl.producer.producingTxn, 1) // allow produces for txns now
	cl.cfg.logger.Log(LogLevelInfo, "beginning transaction", "transactional_id", *cl.cfg.txnID)

	return nil
}

// AbortBufferedRecords fails all unflushed records with ErrAborted and waits
// for there to be no buffered records.
//
// This accepts a context to quit the wait early, but it is strongly
// recommended to always wait for all records to be flushed. Waits should not
// occur. The only case where this function returns an error is if the context
// is canceled while flushing.
//
// The intent of this function is to provide a way to clear the client's
// production backlog. For example, before aborting a transaction and
// beginning a new one, it would be erroneous to not wait for the backlog to
// clear before beginning a new transaction. Anything not cleared may be a part
// of the new transaction.
//
// Records produced during or after a call to this function may not be failed,
// thus it is incorrect to concurrently produce with this function.
//
// This function is safe to call multiple times concurrently, and safe to call
// concurrent with Flush.
func (cl *Client) AbortBufferedRecords(ctx context.Context) error {
	atomic.AddInt32(&cl.producer.aborting, 1)
	defer atomic.AddInt32(&cl.producer.aborting, -1)

	cl.cfg.logger.Log(LogLevelInfo, "producer state set to aborting; continuing to wait via flushing")
	defer cl.cfg.logger.Log(LogLevelDebug, "aborted buffered records")

	// Setting the aborting state allows records to fail before
	// or after produce requests; thus, now we just flush.
	return cl.Flush(ctx)
}

// EndTransaction ends a transaction and resets the client's internal state to
// not be in a transaction.
//
// Flush and CommitOffsetsForTransaction must be called before this function;
// this function does not flush and does not itself ensure that all buffered
// records are flushed. If no record yet has caused a partition to be added to
// the transaction, this function does nothing and returns nil. Alternatively,
// AbortBufferedRecords should be called before aborting a transaction to
// ensure that any buffered records not yet flushed will not be a part of a new
// transaction.
//
// If the producer ID has an error and you are trying to commit, this will
// return with kerr.OperationNotAttempted. If this happened, retry
// EndTransaction with TryAbort. Not other error is retriable, and you should
// not retry with TryAbort.
//
// If records failed with UnknownProducerID and your Kafka version is at least
// 2.5.0, then aborting here will potentially allow the client to recover for
// more production.
//
// Note that canceling the context will likely leave the client in an
// undesirable state, because canceling the context may cancel the in-flight
// EndTransaction request, making it impossible to know whether the commit or
// abort was successful. It is recommended to not cancel the context.
func (cl *Client) EndTransaction(ctx context.Context, commit TransactionEndTry) error {
	cl.producer.txnMu.Lock()
	defer cl.producer.txnMu.Unlock()

	atomic.StoreUint32(&cl.producer.producingTxn, 0) // forbid any new produces while ending txn

	// anyAdded tracks if any partitions were added to this txn, because
	// any partitions written to triggers AddPartitionToTxn, which triggers
	// the txn to actually begin within Kafka.
	//
	// If we consumed at all but did not produce, the transaction ending
	// issues AddOffsetsToTxn, which internally adds a __consumer_offsets
	// partition to the transaction. Thus, if we added offsets, then we
	// also produced.
	var anyAdded bool
	if g := cl.consumer.g; g != nil {
		if g.offsetsAddedToTxn {
			g.offsetsAddedToTxn = false
			anyAdded = true
		}
	} else {
		cl.cfg.logger.Log(LogLevelDebug, "transaction ending, no group loaded; this must be a producer-only transaction, not consume-modify-produce EOS")
	}

	if !cl.producer.inTxn {
		return nil
	}
	cl.producer.inTxn = false

	// After the flush, no records are being produced to, and we can set
	// addedToTxn to false outside of any mutex.
	for _, parts := range cl.producer.topics.load() {
		for _, part := range parts.load().partitions {
			if part.records.addedToTxn {
				part.records.addedToTxn = false
				anyAdded = true
			}
		}
	}

	// If no partition was added to a transaction, then we have nothing to commit.
	//
	// Note that anyAdded is true if the producer ID was failed, meaning we will
	// get to the potential recovery logic below if necessary.
	if !anyAdded {
		cl.cfg.logger.Log(LogLevelInfo, "no records were produced during the commit; thus no transaction was began; ending without doing anything")
		return nil
	}

	id, epoch, err := cl.producerID()
	if err != nil {
		if commit {
			return kerr.OperationNotAttempted
		}

		// If we recovered the producer ID, we return early, since
		// there is no reason to issue an abort now that the id is
		// different. Otherwise, we issue our EndTxn which will likely
		// fail, but that is ok, we will just return error.
		_, didRecover, _ := cl.maybeRecoverProducerID()
		if didRecover {
			return nil
		}
	}

	cl.cfg.logger.Log(LogLevelInfo, "ending transaction",
		"transactional_id", *cl.cfg.txnID,
		"producer_id", id,
		"epoch", epoch,
		"commit", commit,
	)

	err = cl.doWithConcurrentTransactions("EndTxn", func() error {
		req := kmsg.NewPtrEndTxnRequest()
		req.TransactionalID = *cl.cfg.txnID
		req.ProducerID = id
		req.ProducerEpoch = epoch
		req.Commit = bool(commit)
		resp, err := req.RequestWith(ctx, cl)
		if err != nil {
			return err
		}
		return kerr.ErrorForCode(resp.ErrorCode)
	})

	// If the returned error is still a Kafka error, this is fatal and we
	// need to fail our producer ID we loaded above.
	var ke *kerr.Error
	if errors.As(err, &ke) && !ke.Retriable {
		cl.failProducerID(id, epoch, err)
	}

	return err
}

// This returns if it is necessary to recover the producer ID (it has an
// error), whether it is possible to recover, and, if not, the error.
//
// We call this when beginning a transaction or when ending with an abort.
func (cl *Client) maybeRecoverProducerID() (necessary, did bool, err error) {
	id, epoch, err := cl.producerID()
	if err == nil {
		return false, false, nil
	}

	var ke *kerr.Error
	if ok := errors.As(err, &ke); !ok {
		return true, false, err
	}

	kip360 := cl.producer.idVersion >= 3 && (errors.Is(ke, kerr.UnknownProducerID) || errors.Is(ke, kerr.InvalidProducerIDMapping))
	kip588 := cl.producer.idVersion >= 4 && errors.Is(ke, kerr.InvalidProducerEpoch /* || err == kerr.TransactionTimedOut when implemented in Kafka */)

	recoverable := kip360 || kip588
	if !recoverable {
		return true, false, err // fatal, unrecoverable
	}

	// Storing errReloadProducerID will reset sequence numbers as appropriate
	// when the producer ID is reloaded successfully.
	cl.producer.id.Store(&producerID{
		id:    id,
		epoch: epoch,
		err:   errReloadProducerID,
	})
	return true, true, nil
}

// If a transaction is begun too quickly after finishing an old transaction,
// Kafka may still be finalizing its commit / abort and will return a
// concurrent transactions error. We handle that by retrying for a bit.
func (cl *Client) doWithConcurrentTransactions(name string, fn func() error) error {
	start := time.Now()
	tries := 0
	backoff := cl.cfg.txnBackoff
start:
	err := fn()
	if errors.Is(err, kerr.ConcurrentTransactions) && time.Since(start) < 5*time.Second {
		tries++
		cl.cfg.logger.Log(LogLevelDebug, fmt.Sprintf("%s failed with CONCURRENT_TRANSACTIONS, which may be because we ended a txn and began producing in a new txn too quickly; backing off and retrying", name),
			"backoff", backoff,
			"since_request_tries_start", time.Since(start),
			"tries", tries,
		)
		select {
		case <-time.After(backoff):
		case <-cl.ctx.Done():
			cl.cfg.logger.Log(LogLevelError, fmt.Sprintf("abandoning %s retry due to client ctx quitting", name))
			return err
		}
		goto start
	}
	return err
}

////////////////////////////////////////////////////////////////////////////////////////////
// TRANSACTIONAL COMMITTING                                                               //
// MOSTLY DUPLICATED CODE DUE TO NO GENERICS AND BECAUSE THE TYPES ARE SLIGHTLY DIFFERENT //
////////////////////////////////////////////////////////////////////////////////////////////

// commitTransactionOffsets is exactly like CommitOffsets, but specifically for
// use with transactional consuming and producing.
//
// Since this function is a gigantic footgun if not done properly, we hide this
// and only allow transaction sessions to commit.
//
// Unlike CommitOffsets, we do not update the group's uncommitted map. We leave
// that to the calling code to do properly with SetOffsets depending on whether
// an eventual abort happens or not.
func (cl *Client) commitTransactionOffsets(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*kmsg.TxnOffsetCommitRequest, *kmsg.TxnOffsetCommitResponse, error),
) *groupConsumer {
	cl.cfg.logger.Log(LogLevelDebug, "in commitTransactionOffsets", "with", uncommitted)
	defer cl.cfg.logger.Log(LogLevelDebug, "left commitTransactionOffsets")

	if cl.cfg.txnID == nil {
		onDone(nil, nil, errNotTransactional)
		return nil
	}

	// Before committing, ensure we are at least in a transaction. We
	// unlock the producer txnMu before committing to allow EndTransaction
	// to go through, even though that could cut off our commit.
	cl.producer.txnMu.Lock()
	if !cl.producer.inTxn {
		onDone(nil, nil, errNotInTransaction)
		cl.producer.txnMu.Unlock()
		return nil
	}
	cl.producer.txnMu.Unlock()

	g := cl.consumer.g
	if g == nil {
		onDone(kmsg.NewPtrTxnOffsetCommitRequest(), kmsg.NewPtrTxnOffsetCommitResponse(), errNotGroup)
		return nil
	}
	if len(uncommitted) == 0 {
		onDone(kmsg.NewPtrTxnOffsetCommitRequest(), kmsg.NewPtrTxnOffsetCommitResponse(), nil)
		return g
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.offsetsAddedToTxn {
		if err := cl.addOffsetsToTxn(g.ctx, g.cfg.group); err != nil {
			if onDone != nil {
				onDone(nil, nil, err)
			}
			return g
		}
		g.offsetsAddedToTxn = true
	}

	g.commitTxn(ctx, uncommitted, onDone)
	return g
}

// Ties a transactional producer to a group. Since this requires a producer ID,
// this initializes one if it is not yet initialized. This would only be the
// case if trying to commit before any records have been sent.
func (cl *Client) addOffsetsToTxn(ctx context.Context, group string) error {
	id, epoch, err := cl.producerID()
	if err != nil {
		return err
	}

	err = cl.doWithConcurrentTransactions("AddOffsetsToTxn", func() error { // committing offsets without producing causes a transaction to begin within Kafka
		cl.cfg.logger.Log(LogLevelInfo, "issuing AddOffsetsToTxn",
			"txn", *cl.cfg.txnID,
			"producerID", id,
			"producerEpoch", epoch,
			"group", group,
		)
		req := kmsg.NewPtrAddOffsetsToTxnRequest()
		req.TransactionalID = *cl.cfg.txnID
		req.ProducerID = id
		req.ProducerEpoch = epoch
		req.Group = group
		resp, err := req.RequestWith(ctx, cl)
		if err != nil {
			return err
		}
		return kerr.ErrorForCode(resp.ErrorCode)
	})

	// If the returned error is still a Kafka error, this is fatal and we
	// need to fail our producer ID we created just above.
	var ke *kerr.Error
	if errors.As(err, &ke) && !ke.Retriable {
		cl.failProducerID(id, epoch, err)
	}

	return err
}

// commitTxn is ALMOST EXACTLY THE SAME as commit, but changed for txn types
// and we avoid updateCommitted. We avoid updating because we manually
// SetOffsets when ending the transaction.
func (g *groupConsumer) commitTxn(
	ctx context.Context,
	uncommitted map[string]map[int32]EpochOffset,
	onDone func(*kmsg.TxnOffsetCommitRequest, *kmsg.TxnOffsetCommitResponse, error),
) {
	if onDone == nil { // note we must always call onDone
		onDone = func(_ *kmsg.TxnOffsetCommitRequest, _ *kmsg.TxnOffsetCommitResponse, _ error) {}
	}
	if len(uncommitted) == 0 { // only empty if called thru autocommit / default revoke
		onDone(kmsg.NewPtrTxnOffsetCommitRequest(), kmsg.NewPtrTxnOffsetCommitResponse(), nil)
		return
	}

	if g.commitCancel != nil {
		g.commitCancel() // cancel any prior commit
	}
	priorCancel := g.commitCancel
	priorDone := g.commitDone

	// Unlike the non-txn consumer, we use the group context for
	// transaction offset committing. We want to quit when the group is
	// left, and we are not committing when leaving. We rely on proper
	// usage of the GroupTransactSession API to issue commits, so there is
	// no reason not to use the group context here.
	commitCtx, commitCancel := context.WithCancel(g.ctx) // enable ours to be canceled and waited for
	commitDone := make(chan struct{})

	g.commitCancel = commitCancel
	g.commitDone = commitDone

	// We issue this request even if the producer ID is failed; the request
	// will fail if it is.
	//
	// The id must have been set at least once by this point because of
	// addOffsetsToTxn.
	id, epoch, _ := g.cl.producerID()
	req := kmsg.NewPtrTxnOffsetCommitRequest()
	req.TransactionalID = *g.cl.cfg.txnID
	req.Group = g.cfg.group
	req.ProducerID = id
	req.ProducerEpoch = epoch
	req.Generation = g.generation
	req.MemberID = g.memberID
	req.InstanceID = g.cfg.instanceID

	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				commitCancel()
			case <-commitCtx.Done():
			}
		}()
	}

	go func() {
		defer close(commitDone) // allow future commits to continue when we are done
		defer commitCancel()
		if priorDone != nil {
			select {
			case <-priorDone:
			default:
				g.cl.cfg.logger.Log(LogLevelDebug, "canceling prior txn offset commit to issue another")
				priorCancel()
				<-priorDone // wait for any prior request to finish
			}
		}
		g.cl.cfg.logger.Log(LogLevelDebug, "issuing txn offset commit", "uncommitted", uncommitted)

		for topic, partitions := range uncommitted {
			reqTopic := kmsg.NewTxnOffsetCommitRequestTopic()
			reqTopic.Topic = topic
			for partition, eo := range partitions {
				reqPartition := kmsg.NewTxnOffsetCommitRequestTopicPartition()
				reqPartition.Partition = partition
				reqPartition.Offset = eo.Offset
				reqPartition.LeaderEpoch = eo.Epoch
				reqPartition.Metadata = &req.MemberID
				reqTopic.Partitions = append(reqTopic.Partitions, reqPartition)
			}
			req.Topics = append(req.Topics, reqTopic)
		}

		var resp *kmsg.TxnOffsetCommitResponse
		var err error
		if len(req.Topics) > 0 {
			resp, err = req.RequestWith(commitCtx, g.cl)
		}
		if err != nil {
			onDone(req, nil, err)
			return
		}
		onDone(req, resp, nil)
	}()
}
